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

	imgFile *os.File
	size    uint64

	hub      *Hub
	tracker  Tracker
	preserve bool

	detector TSDetector

	tsMu sync.Mutex

	activeTSStream string

	/*
		Authoritative volatile disk image.
	*/
	shm *SHMDisk

	/*
		Asynchronous physical persistence.
	*/
	writer *DiskWriter

	/*
		Serialize logical filesystem ordering.

		This lock is never held while waiting for physical disk I/O.
	*/
	logicalMu sync.Mutex
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

	n, err := d.shm.ReadAt(
		data,
		off,
	)

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

	if len(data) == 0 {
		return 0, 0
	}

	start := uint64(off)

	if uint64(len(data)) > ^uint64(0)-start {
		return 0, syscall.EFBIG
	}

	end := start + uint64(len(data))

	if end > d.size {
		return 0, syscall.EFBIG
	}

	select {
	case <-ctx.Done():
		return 0, syscall.EINTR
	default:
	}

	d.logicalMu.Lock()
	defer d.logicalMu.Unlock()

	if !d.writer.Accepting() {
		return 0, syscall.EIO
	}

	/*
		Stage the whole FUSE write into /dev/shm.

		One generation covers the entire write. Physical plans below
		refer to subranges of that generation.
	*/
	staged, err := d.shm.StageWrite(
		data,
		start,
	)
	if err != nil {
		log.Printf(
			"SHM stage failed off=%d len=%d: %v",
			start,
			len(data),
			err,
		)

		return 0, toErrno(err)
	}

	segs := d.tracker.Classify(
		start,
		uint64(len(data)),
	)

	if len(segs) == 0 {
		_ = d.shm.ReleaseIfCurrent(
			[]ShmExtent{staged},
		)

		log.Printf(
			"tracker returned no ranges for write off=%d len=%d",
			start,
			len(data),
		)

		return 0, syscall.EIO
	}

	if verbLog {
		log.Printf(
			"write off=%d len=%d segs=%d",
			start,
			len(data),
			len(segs),
		)
	}

	plans := make(
		[]diskWritePlan,
		0,
		len(segs),
	)

	for _, seg := range segs {
		if seg.End <= seg.Start {
			continue
		}

		if seg.Start < start || seg.End > end {
			_ = d.shm.ReleaseIfCurrent(
				[]ShmExtent{staged},
			)

			log.Printf(
				"invalid tracker range: write=[%d,%d) seg=[%d,%d)",
				start,
				end,
				seg.Start,
				seg.End,
			)

			return 0, syscall.EIO
		}

		persist := true
		label := "DATA"

		if seg.Kind == RangeCandidate {
			persist = d.preserve
			label = "CANDIDATE"
		}

		plans = append(
			plans,
			diskWritePlan{
				start: seg.Start,
				end:   seg.End,

				persist: persist,

				overlay: []ShmExtent{
					{
						Start: seg.Start,
						End:   seg.End,
						Gen:   staged.Gen,
					},
				},

				label: label,
			},
		)
	}

	/*
		Advance the logical filesystem / TS state immediately.
	*/
	var punches []ByteRange

	for _, seg := range segs {
		if seg.End <= seg.Start {
			continue
		}

		a := int(seg.Start - start)
		b := int(seg.End - start)

		part := data[a:b]

		switch seg.Kind {
		case RangeCandidate:
			streamID := seg.StreamID
			if streamID == "" {
				streamID = "default"
			}

			d.feedCandidate(
				streamID,
				seg.StreamOffset,
				part,
			)

		case RangeMeta, RangeNormal, RangeUnknown:
			newPunches, errno :=
				d.processMetadataWrite(
					seg.Start,
					seg.End-seg.Start,
				)

			if errno != 0 {
				log.Printf(
					"logical metadata processing failed off=%d len=%d: %v",
					seg.Start,
					seg.End-seg.Start,
					errno,
				)
			}

			punches = append(
				punches,
				newPunches...,
			)
		}
	}

	punches = coalescePunchRanges(punches)

	punchPlans := make(
		[]diskPunchPlan,
		0,
		len(punches),
	)

	for _, r := range punches {
		if r.End <= r.Start {
			continue
		}

		punchPlans = append(
			punchPlans,
			diskPunchPlan{
				start: r.Start,
				end:   r.End,

				overlay: d.shm.Snapshot(
					r.Start,
					r.End,
				),
			},
		)
	}

	/*
		Only metadata goes into the physical queue.
	*/
	if err := d.writer.Enqueue(
		diskWriteOp{
			writes:  plans,
			punches: coalescePunchPlans(punchPlans),
		},
	); err != nil {
		_ = d.shm.ReleaseIfCurrent(
			[]ShmExtent{staged},
		)

		log.Printf(
			"queue physical write failed: %v",
			err,
		)

		return 0, toErrno(err)
	}

	return uint32(len(data)), 0
}

func (d *DiskNode) Flush(
	ctx context.Context,
	fh fs.FileHandle,
) syscall.Errno {
	return d.syncPhysical(ctx)
}

func (d *DiskNode) Fsync(
	ctx context.Context,
	fh fs.FileHandle,
	flags uint32,
) syscall.Errno {
	return d.syncPhysical(ctx)
}

func (d *DiskNode) syncPhysical(
	ctx context.Context,
) syscall.Errno {
	start := time.Now()

	/*
		Sync() queues the barrier in the physical writer after all
		operations which were logically submitted before it.

		It may block here because an explicit Fsync is intentionally
		allowed to wait for the physical disk.
	*/
	d.logicalMu.Lock()
	err := d.writer.Sync(ctx)
	d.logicalMu.Unlock()

	delay := time.Since(start)

	if delay > 20*time.Millisecond {
		log.Printf(
			"slow queued sync took=%v",
			delay,
		)
	}

	if err == context.Canceled ||
		err == context.DeadlineExceeded {
		return syscall.EINTR
	}

	return toErrno(err)
}
