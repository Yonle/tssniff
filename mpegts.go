package main

import (
	"log"
	"sync"
)

const (
	tsPacketSize = 188
	tsSyncByte   = 0x47

	// Number of consecutive MPEG-TS packets required before a candidate
	// is accepted as a transport stream.
	minTSPackets = 10

	// Maximum number of packets emitted in one Hub.Broadcast().
	maxBroadcastPackets = 128
)

type TSDetector struct {
	mu sync.Mutex

	streamID string

	haveOffset bool
	nextOffset uint64

	detected bool

	buffer []byte
}

func (d *TSDetector) resetLocked() {
	d.streamID = ""

	d.haveOffset = false
	d.nextOffset = 0

	d.detected = false

	d.buffer = d.buffer[:0]
}

func (d *TSDetector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.resetLocked()
}

/*
Feed receives candidate file-data bytes.

	streamID      identifies the logical recording/file.
	logicalOffset identifies where these bytes belong inside that stream.

For FAT32, logicalOffset is currently the physical disk offset.

For NTFS, logicalOffset is the file's logical byte offset, allowing
physical fragmentation without confusing it with a new stream.
*/
func (d *TSDetector) Feed(
	streamID string,
	logicalOffset uint64,
	data []byte,
	emit func([]byte),
) {
	if len(data) == 0 {
		return
	}

	if streamID == "" {
		streamID = "default"
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	/*
		A different stream means a different recording/file.
	*/
	if d.streamID != streamID {
		if d.detected {
			if verbLog {
				log.Printf(
					"TS detector: ignore other stream=%q active=%q",
					streamID,
					d.streamID,
				)
			}
			return
		}
		d.resetLocked()
	}

	if d.streamID == "" {
		d.streamID = streamID

		if verbLog {
			log.Printf(
				"TS detector: new stream=%q",
				streamID,
			)
		}
	}

	/*
		Do NOT feed backwards filesystem writes into the MPEG-TS
		buffer.

		This is critical for FAT32 because recorder bookkeeping may
		update an earlier physical location while the actual TS data
		is being appended elsewhere.

		Example:

		    TS frontier = 280952832
		    incoming    = 135528448

		That write is not the continuation of the TS stream.
	*/
	if d.haveOffset {
		if logicalOffset < d.nextOffset {
			if verbLog {
				log.Printf(
					"TS detector: skip backwards write stream=%q off=%d frontier=%d",
					streamID,
					logicalOffset,
					d.nextOffset,
				)
			}

			return
		}

		/*
			A forward discontinuity means we don't have the bytes between
			the current frontier and this write.

			Do not stitch them together into one MPEG-TS stream.
		*/
		if logicalOffset > d.nextOffset {
			if verbLog {
				log.Printf(
					"TS detector: forward gap stream=%q off=%d frontier=%d, reset",
					streamID,
					logicalOffset,
					d.nextOffset,
				)
			}

			d.buffer = d.buffer[:0]
			d.detected = false
		}
	}

	d.haveOffset = true
	d.nextOffset = logicalOffset + uint64(len(data))

	d.buffer = append(d.buffer, data...)

	/*
		Before MPEG-TS detection, look for the actual 188-byte sync
		pattern:

		    47 ...........
		    47 ...........
		    47 ...........
		    ...

		This prevents arbitrary file data from being treated as TS.
	*/
	if !d.detected {
		idx, ok := findMPEGTSOffset(d.buffer)

		if !ok {
			/*
				Keep enough tail bytes so a sync pattern may span
				two separate filesystem writes.
			*/
			const keep = tsPacketSize*minTSPackets - 1

			if len(d.buffer) > keep {
				drop := len(d.buffer) - keep

				copy(
					d.buffer,
					d.buffer[drop:],
				)

				d.buffer = d.buffer[:keep]
			}

			return
		}

		if idx > 0 {
			d.buffer = d.buffer[idx:]
		}

		d.detected = true

		if verbLog {
			log.Printf(
				"TS detector: MPEG-TS detected stream=%q",
				streamID,
			)
		}
	}

	/*
		Once detected, emit complete MPEG-TS packets only.
	*/
	for len(d.buffer) >= tsPacketSize {
		if !validTSPacket(d.buffer) {
			idx, ok := findMPEGTSOffset(d.buffer)

			if !ok {
				/*
					Keep a tail for possible resynchronisation on the
					next sequential write.
				*/
				keep := tsPacketSize*minTSPackets - 1

				if len(d.buffer) > keep {
					drop := len(d.buffer) - keep

					copy(
						d.buffer,
						d.buffer[drop:],
					)

					d.buffer = d.buffer[:keep]
				}

				return
			}

			if idx > 0 {
				d.buffer = d.buffer[idx:]
			}

			if len(d.buffer) < tsPacketSize {
				return
			}
		}

		packetCount := len(d.buffer) / tsPacketSize

		if packetCount > maxBroadcastPackets {
			packetCount = maxBroadcastPackets
		}

		validPackets := 0

		for i := 0; i < packetCount; i++ {
			off := i * tsPacketSize

			if !validTSPacket(
				d.buffer[off:],
			) {
				break
			}

			validPackets++
		}

		if validPackets == 0 {
			d.buffer = d.buffer[1:]
			continue
		}

		n := validPackets * tsPacketSize

		payload := make(
			[]byte,
			n,
		)

		copy(
			payload,
			d.buffer[:n],
		)

		if verbLog {
			log.Printf(
				"MPEG-TS broadcast len=%d packets=%d",
				len(payload),
				validPackets,
			)
		}

		emit(payload)

		remaining := len(d.buffer) - n

		if remaining > 0 {
			copy(
				d.buffer,
				d.buffer[n:],
			)
		}

		d.buffer = d.buffer[:remaining]
	}
}

/*
validTSPacket performs a minimal MPEG-TS header sanity check.

MPEG-TS:

	byte 0:
	    0x47 sync byte

	byte 3:
	    adaptation_field_control must not be 00.
*/
func validTSPacket(data []byte) bool {
	if len(data) < tsPacketSize {
		return false
	}

	if data[0] != tsSyncByte {
		return false
	}

	// adaptation_field_control == 00 is reserved.
	if data[3]&0x30 == 0 {
		return false
	}

	return true
}

/*
findMPEGTSOffset searches for a valid MPEG-TS sync position.

A valid candidate must contain minTSPackets consecutive packet positions
with a 0x47 sync byte and valid MPEG-TS headers.
*/
func findMPEGTSOffset(data []byte) (int, bool) {
	need := tsPacketSize * minTSPackets

	if len(data) < need {
		return 0, false
	}

	limit := len(data) - need

	for i := 0; i <= limit; i++ {
		if data[i] != tsSyncByte {
			continue
		}

		ok := true

		for j := 0; j < minTSPackets; j++ {
			off := i + j*tsPacketSize

			if !validTSPacket(data[off:]) {
				ok = false
				break
			}
		}

		if ok {
			return i, true
		}
	}

	return 0, false
}

func scanTSPayload(data []byte) (int, bool) {
	return findMPEGTSOffset(data)
}
