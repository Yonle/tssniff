package main

import (
	"context"
	"io"
	"log"
	"os"
	"sync"
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
	tracker  Tracker
	preserve bool

	detector TSDetector
	tsMu     sync.Mutex
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

	if verbLog {
		log.Printf(
			"read off=%d len=%d",
			start,
			length,
		)
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

	if uint64(len(data)) > ^uint64(0)-start {
		return 0, syscall.EFBIG
	}

	end := start + uint64(len(data))

	if end > d.size {
		return 0, syscall.EFBIG
	}

	if len(data) == 0 {
		return 0, 0
	}

	segs := d.tracker.Classify(
		start,
		uint64(len(data)),
	)

	if verbLog {
		log.Printf(
			"write off=%d len=%d segs=%d",
			start,
			len(data),
			len(segs),
		)
	}

	for _, seg := range segs {
		if seg.End <= seg.Start {
			continue
		}

		if seg.Start < start || seg.End > end {
			log.Printf(
				"invalid tracker range: write=[%d,%d) seg=[%d,%d)",
				start,
				end,
				seg.Start,
				seg.End,
			)
			return 0, syscall.EIO
		}

		rel := seg.Start - start
		partLen := seg.End - seg.Start

		part := data[int(rel):int(rel+partLen)]

		switch seg.Kind {
		case RangeCandidate:
			if err := d.writeBacking(
				part,
				seg.Start,
				"CANDIDATE",
			); err != 0 {
				return uint32(rel), err
			}

			streamID := seg.StreamID
			if streamID == "" {
				streamID = "default"
			}

			if verbLog {
				first := byte(0)
				if len(part) > 0 {
					first = part[0]
				}

				log.Printf(
					"TS candidate phys=%d len=%d stream=%q first=%02x",
					seg.Start,
					len(part),
					streamID,
					first,
				)
			}

			d.feedCandidate(
				streamID,
				seg.StreamOffset,
				part,
			)
		case RangeMeta, RangeNormal, RangeUnknown:
			if err := d.writeBacking(
				part,
				seg.Start,
				"DATA",
			); err != 0 {
				return uint32(rel), err
			}

			if d.tracker.InRootDir(seg.Start) {
				if verbLog {
					log.Printf(
						"root directory write at %d: reset TS detector",
						seg.Start,
					)
				}

				d.detector.Reset()
			}

			newCandidates := d.tracker.OnMetadataWrite(
				seg.Start,
				partLen,
			)

			if len(newCandidates) > 0 {
				if verbLog {
					log.Printf(
						"NTFS: %d newly discovered candidate range(s)",
						len(newCandidates),
					)
				}

				if err := d.replayCandidateRanges(
					newCandidates,
				); err != 0 {
					return uint32(rel), err
				}
			}
		}
	}

	return uint32(len(data)), 0
}

func (d *DiskNode) writeBacking(
	data []byte,
	off uint64,
	kind string,
) syscall.Errno {
	start := time.Now()

	n, err := d.imgFile.WriteAt(
		data,
		int64(off),
	)

	delay := time.Since(start)

	if delay > 20*time.Millisecond {
		log.Printf(
			"slow %s write off=%d len=%d took=%v",
			kind,
			off,
			len(data),
			delay,
		)
	}

	if err != nil {
		return toErrno(err)
	}

	if n != len(data) {
		return syscall.EIO
	}

	return 0
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

	err := toErrno(
		d.imgFile.Sync(),
	)

	delay := time.Since(start)

	if delay > 20*time.Millisecond {
		log.Printf(
			"slow sync took=%v",
			delay,
		)
	}

	return err
}

func (d *DiskNode) replayCandidateRanges(
	ranges []ByteRange,
) syscall.Errno {
	const chunkSize = 1 << 20 // 1 MiB

	for _, r := range ranges {
		if r.End <= r.Start {
			continue
		}

		if r.Kind != RangeCandidate {
			continue
		}

		streamID := r.StreamID
		if streamID == "" {
			streamID = "default"
		}

		remaining := r.End - r.Start
		phys := r.Start
		logical := r.StreamOffset

		d.tsMu.Lock()
		defer d.tsMu.Unlock()

		for remaining > 0 {
			n := uint64(chunkSize)
			if n > remaining {
				n = remaining
			}

			buf := make([]byte, int(n))

			readN, err := d.imgFile.ReadAt(
				buf,
				int64(phys),
			)

			if err != nil && err != io.EOF {
				log.Printf(
					"NTFS replay read failed phys=%d len=%d: %v",
					phys,
					n,
					err,
				)
				return toErrno(err)
			}

			if readN == 0 {
				break
			}

			if verbLog {
				log.Printf(
					"NTFS replay phys=%d logical=%d len=%d stream=%q",
					phys,
					logical,
					readN,
					streamID,
				)
			}
			d.detector.Feed(
				streamID,
				logical,
				buf[:readN],
				func(payload []byte) {
					if verbLog {
						log.Printf(
							"MPEG-TS broadcast len=%d",
							len(payload),
						)
					}

					d.hub.Broadcast(payload)
				},
			)
			phys += uint64(readN)
			logical += uint64(readN)
			remaining -= uint64(readN)

			if readN < int(n) {
				break
			}
		}
	}

	return 0
}

func (d *DiskNode) feedCandidate(
	streamID string,
	streamOffset uint64,
	data []byte,
) {
	d.tsMu.Lock()
	defer d.tsMu.Unlock()

	d.detector.Feed(
		streamID,
		streamOffset,
		data,
		func(payload []byte) {
			if verbLog {
				log.Printf(
					"MPEG-TS broadcast len=%d",
					len(payload),
				)
			}

			d.hub.Broadcast(payload)
		},
	)
}
