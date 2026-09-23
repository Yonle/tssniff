package main

import (
	"context"
	"io"
	"log"
	"os"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type DiskNode struct {
	fs.Inode

	imgFile  *os.File
	size     uint64
	hub      *Hub
	tracker  *FSTracker
	preserve bool
	shm      *ShmBuffer

	tsBuf      []byte
	tsNextOff  uint64
	tsHaveData bool
}

var (
	_ fs.NodeGetattrer = (*DiskNode)(nil)
	_ fs.NodeOpener    = (*DiskNode)(nil)
	_ fs.NodeReader    = (*DiskNode)(nil)
	_ fs.NodeWriter    = (*DiskNode)(nil)
	_ fs.NodeFlusher   = (*DiskNode)(nil)
	_ fs.NodeFsyncer   = (*DiskNode)(nil)
)

func (d *DiskNode) Getattr(
	ctx context.Context,
	fh fs.FileHandle,
	out *fuse.AttrOut,
) syscall.Errno {
	out.Mode = syscall.S_IFREG | 0644
	out.Size = d.size
	out.Blksize = 512
	out.Blocks = d.size / 512
	return 0
}

func (d *DiskNode) Open(
	ctx context.Context,
	flags uint32,
) (fs.FileHandle, uint32, syscall.Errno) {
	return nil, fuse.FOPEN_DIRECT_IO, 0
}

func (d *DiskNode) Read(
	ctx context.Context,
	fh fs.FileHandle,
	dest []byte,
	off int64,
) (fuse.ReadResult, syscall.Errno) {
	if off < 0 {
		return fuse.ReadResultData(nil), syscall.EINVAL
	}
	start := uint64(off)
	if start >= d.size || len(dest) == 0 {
		return fuse.ReadResultData(nil), 0
	}
	length := uint64(len(dest))
	if length > d.size-start {
		length = d.size - start
	}
	data := dest[:int(length)]

	n, err := d.imgFile.ReadAt(data, off)
	if err != nil && err != io.EOF {
		return fuse.ReadResultData(nil), toErrno(err)
	}
	if n < len(data) {
		clear(data[n:])
	}

	metaEnd := d.tracker.MetadataEnd()
	tsBytes := uint64(0)
	for i := uint64(0); i < length; i++ {
		if start+i >= metaEnd {
			data[i] = 0
			tsBytes++
		}
	}

	if verbLog {
		log.Printf("read off=%d len=%d metaEnd=%d tsZeroed=%d",
			start, length, metaEnd, tsBytes)
	}

	return fuse.ReadResultData(data), 0
}

func (d *DiskNode) Write(
	ctx context.Context,
	fh fs.FileHandle,
	data []byte,
	off int64,
) (uint32, syscall.Errno) {
	if off < 0 {
		return 0, syscall.EINVAL
	}
	start := uint64(off)
	end := start + uint64(len(data))
	if end > d.size {
		return 0, syscall.EFBIG
	}
	if len(data) == 0 {
		return 0, 0
	}

	segs := d.tracker.Classify(start, uint64(len(data)))

	if verbLog {
		log.Printf("write off=%d len=%d metaEnd=%d segs=%d",
			start, len(data), d.tracker.MetadataEnd(), len(segs))
	}

	for _, seg := range segs {
		rel := seg.Start - start
		part := data[rel : rel+(seg.End-seg.Start)]

		if verbLog {
			kind := "META"
			if seg.Kind == RangeTS {
				kind = "TS"
			}
			log.Printf("  seg off=%d len=%d kind=%s",
				seg.Start, len(part), kind)
		}

		if seg.Kind == RangeTS {
			d.broadcastTS(part, seg.Start)
		} else {
			start := time.Now()
			n, err := d.imgFile.WriteAt(part, int64(seg.Start))
			dely := time.Since(start)
			if dely > 20*time.Millisecond {
				log.Printf("slow META write off=%d len=%d took=%v", seg.Start, len(part), dely)
			}
			if err != nil {
				return uint32(rel), toErrno(err)
			}
			if n != len(part) {
				return uint32(rel + uint64(n)), syscall.EIO
			}

			// A write to the root directory means the STB is
			// creating, updating, or closing a directory entry —
			// i.e. a new recording is starting.  Reset the TS
			// continuity state so the first write of the new
			// recording is accepted regardless of its offset.
			if d.tracker.InRootDir(seg.Start) {
				if verbLog {
					log.Printf("root dir write at %d: reset TS continuity", seg.Start)
				}
				d.tsHaveData = false
			}
		}
	}

	return uint32(len(data)), 0
}

func (d *DiskNode) Flush(
	ctx context.Context,
	fh fs.FileHandle,
) syscall.Errno {
	return d._sync(ctx)
}

func (d *DiskNode) Fsync(
	ctx context.Context,
	fh fs.FileHandle,
	flags uint32,
) syscall.Errno {
	return d._sync(ctx)
}

func (d *DiskNode) _sync(
	ctx context.Context,
) syscall.Errno {
	start := time.Now()
	err := toErrno(d.imgFile.Sync())
	dely := time.Since(start)
	if dely > 20*time.Millisecond {
		log.Printf("slow sync took=%v", dely)
	}
	return err
}

func (d *DiskNode) broadcastTS(data []byte, off uint64) {
	if d.tsHaveData {
		if off < d.tsNextOff {
			// Backwards write: bookkeeping at the file's start.
			// Dropping these is what keeps the stream clean.
			if verbLog {
				log.Printf("broadcastTS: skip backwards off=%d (frontier %d)",
					off, d.tsNextOff)
			}
			return
		}
		if off > d.tsNextOff {
			// Forward jump: a new recording started.  Flush any
			// partial packet left from the previous file and start
			// the buffer over.
			if verbLog {
				log.Printf("broadcastTS: forward jump off=%d (was %d), reset",
					off, d.tsNextOff)
			}
			d.tsBuf = d.tsBuf[:0]
		}
		// off == tsNextOff: normal continuation, fall through.
	}

	d.tsNextOff = off + uint64(len(data))
	d.tsHaveData = true

	d.tsBuf = append(d.tsBuf, data...)

	for len(d.tsBuf) >= tsPacketSize {
		if d.tsBuf[0] != tsSyncByte {
			idx, ok := findMPEGTSOffset(d.tsBuf)
			if !ok {
				keep := tsPacketSize - 1
				if len(d.tsBuf) > keep {
					copy(d.tsBuf, d.tsBuf[len(d.tsBuf)-keep:])
					d.tsBuf = d.tsBuf[:keep]
				}
				if verbLog {
					log.Printf("broadcastTS: no resync, kept %d bytes", len(d.tsBuf))
				}
				return
			}
			d.tsBuf = d.tsBuf[idx:]
		}

		valid := 0
		for i := 0; i+tsPacketSize <= len(d.tsBuf); i += tsPacketSize {
			if d.tsBuf[i] != tsSyncByte {
				break
			}
			valid++
		}
		if valid == 0 {
			d.tsBuf = d.tsBuf[1:]
			continue
		}

		n := valid * tsPacketSize
		payload := make([]byte, n)
		copy(payload, d.tsBuf[:n])

		d.hub.Broadcast(payload)
		if d.preserve && d.shm != nil {
			if _, err := d.shm.Write(payload); err != nil && verbLog {
				log.Printf("shm write: %v", err)
			}
		}

		rem := len(d.tsBuf) - n
		copy(d.tsBuf, d.tsBuf[n:])
		d.tsBuf = d.tsBuf[:rem]
	}
}
