package main

import (
	"errors"
	"log"
	"os"
	"sync"
)

type NBDBackend struct {
	mu       sync.RWMutex
	imgFile  *os.File
	size     int64
	hub      *Hub
	tracker  *FSTracker
	preserve bool
	shm      *ShmBuffer

	tsBuf []byte

	// TS reassembly state.
	frontier     uint64
	initialized  bool
	pending      map[uint64][]byte
	pendingBytes int
}

const maxPendingBytes = 8 * 1024 * 1024

func NewNBDBackend(
	imgFile *os.File,
	size int64,
	hub *Hub,
	tracker *FSTracker,
	preserve bool,
	shm *ShmBuffer,
) *NBDBackend {
	return &NBDBackend{
		imgFile:  imgFile,
		size:     size,
		hub:      hub,
		tracker:  tracker,
		preserve: preserve,
		shm:      shm,
		pending:  make(map[uint64][]byte),
	}
}

// Size returns the total size of the virtual block device in bytes.
func (b *NBDBackend) Size() (int64, error) {
	return b.size, nil
}

// Sync flushes metadata writes to the sparse image.
func (b *NBDBackend) Sync() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.imgFile.Sync()
}

// ReadAt handles block reads from the NBD device.
// Metadata regions are read from the sparse image.
// TS regions return zeros (the STB should never read back its own recordings).
func (b *NBDBackend) ReadAt(p []byte, off int64) (int, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if off < 0 || off+int64(len(p)) > b.size {
		return 0, errors.New("read out of bounds")
	}

	uoff := uint64(off)
	if b.tracker.IsMetadata(uoff, uint32(len(p))) {
		return b.imgFile.ReadAt(p, off)
	}

	// TS data: return zeros. The STB never reads back what it just recorded.
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

// WriteAt handles block writes from the NBD device.
// Metadata writes go to the sparse image.
func (b *NBDBackend) WriteAt(p []byte, off int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if off < 0 || off+int64(len(p)) > b.size {
		return 0, errors.New("write out of bounds")
	}

	uoff := uint64(off)
	dataLen := uint32(len(p))

	isMetadata := b.tracker.IsMetadata(uoff, dataLen)

	if isMetadata {
		if err := b.overlayWrite(uoff, p); err != nil {
			return 0, err
		}
		return len(p), nil
	}

	// MPEG-TS payload: reassemble by offset and broadcast.
	b.processTSPayload(uoff, p)

	if b.preserve && b.shm != nil {
		if _, err := b.shm.Write(p); err != nil {
			// not fatal
		}
	}

	return len(p), nil
}

// overlayWrite persists metadata writes to the sparse image.
// It does NOT maintain the in-memory overlay map from the FUSE version;
// NBD reads go directly to imgFile for metadata regions.
func (b *NBDBackend) overlayWrite(off uint64, data []byte) error {
	_, err := b.imgFile.WriteAt(data, int64(off))
	return err
}

func (b *NBDBackend) processTSPayload(off uint64, data []byte) {
	if !b.initialized {
		b.frontier = off
		b.initialized = true
	}

	// Fully or partially behind the frontier: already broadcast.
	if off < b.frontier {
		end := off + uint64(len(data))
		if end <= b.frontier {
			return
		}
		skip := b.frontier - off
		off = b.frontier
		data = data[skip:]
	}

	if off == b.frontier {
		b.appendAndDrain(data)
		return
	}

	if verbLog {
		log.Printf("TS reorder: off=%d frontier=%d gap=%d pending=%d",
			off, b.frontier, off-b.frontier, b.pendingBytes)
	}

	// Ahead of the frontier: buffer out-of-order.
	if _, exists := b.pending[off]; exists {
		return
	}
	b.pending[off] = data
	b.pendingBytes += len(data)

	// If pending grows too large, a chunk was likely lost. Advance the
	// frontier to the lowest pending offset so we can keep moving.
	for b.pendingBytes > maxPendingBytes {
		var minOff uint64 = ^uint64(0)
		for o := range b.pending {
			if o < minOff {
				minOff = o
			}
		}
		if minOff == ^uint64(0) {
			break
		}
		b.frontier = minOff
		before := b.pendingBytes
		b.drainPending()
		if b.pendingBytes == before {
			// Safety: drain made no progress; drop one entry.
			if d, ok := b.pending[minOff]; ok {
				delete(b.pending, minOff)
				b.pendingBytes -= len(d)
			}
		}
	}
}

func (b *NBDBackend) appendAndDrain(data []byte) {
	b.tsBuf = append(b.tsBuf, data...)
	b.processTSBuf()
	b.frontier += uint64(len(data))
	b.drainPending()
}

func (b *NBDBackend) drainPending() {
	for {
		data, ok := b.pending[b.frontier]
		if !ok {
			return
		}
		delete(b.pending, b.frontier)
		b.pendingBytes -= len(data)

		b.tsBuf = append(b.tsBuf, data...)
		b.processTSBuf()
		b.frontier += uint64(len(data))
	}
}

// processTSBuf extracts complete TS packets from tsBuf and broadcasts them.
func (b *NBDBackend) processTSBuf() {
	for len(b.tsBuf) >= tsPacketSize {
		if b.tsBuf[0] != tsSyncByte {
			offset, found := findMPEGTSOffset(b.tsBuf)
			if !found {
				if len(b.tsBuf) >= tsPacketSize {
					b.tsBuf = b.tsBuf[len(b.tsBuf)-(tsPacketSize-1):]
				}
				break
			}
			b.tsBuf = b.tsBuf[offset:]
			if len(b.tsBuf) < tsPacketSize {
				break
			}
		}

		validPackets := 0
		for i := 0; i+tsPacketSize <= len(b.tsBuf); i += tsPacketSize {
			if b.tsBuf[i] == tsSyncByte {
				validPackets++
			} else {
				break
			}
		}

		if validPackets == 0 {
			b.tsBuf = b.tsBuf[1:]
			continue
		}

		n := validPackets
		if n > maxBroadcastPackets {
			n = maxBroadcastPackets
		}
		sendLen := n * tsPacketSize

		// Copy before broadcasting; tsBuf is reused below.
		payload := acquirePayload(sendLen)
		copy(payload, b.tsBuf[:sendLen])

		b.hub.Broadcast(payload)

		remainder := len(b.tsBuf) - sendLen
		copy(b.tsBuf, b.tsBuf[sendLen:])
		b.tsBuf = b.tsBuf[:remainder]
	}
}

// Flush drains any remaining TS data in tsBuf.
func (b *NBDBackend) Flush() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.processTSBuf()
}
