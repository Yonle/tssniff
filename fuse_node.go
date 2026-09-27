package main

import (
	"context"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type DiskNode struct {
	fs.Inode

	imgFile *os.File

	/*
		size is read by Getattr and Read on the FUSE goroutine pool,
		and written by Write when the STB extends the volume.
		Use atomic access so the extension is visible without a
		lock on the hot read path.
	*/
	size atomic.Uint64

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
	size := d.size.Load()

	out.Mode = syscall.S_IFREG | 0644
	out.Size = size
	out.Blksize = 512
	out.Blocks = size / 512

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

	size := d.size.Load()

	start := uint64(off)

	if start >= size || len(dest) == 0 {
		return fuse.ReadResultData(nil), 0
	}

	length := uint64(len(dest))

	if length > size-start {
		length = size - start
	}

	data := dest[:int(length)]

	/*
		SHM is a delta layer. It holds only extents that have not
		yet been persisted or punched. Every range that has been
		written through to the sparse image has had its SHM extent
		released.

		Read SHM first, then fall back to the physical image for
		whatever SHM does not cover.
	*/
	n := 0

	for n < len(data) {
		chunkOff := off + int64(n)

		chunk := data[n:]

		t0 := time.Now()
		shmN, shmErr := d.shm.ReadAt(
			chunk,
			chunkOff,
		)
		shmDur := time.Since(t0)

		if shmN > 0 {
			n += shmN

			if shmErr == nil {
				continue
			}
		}

		/*
			Either SHM had no extent here, or it errored.
			Fall through to the physical image for the rest of
			the requested range.
		*/

		t1 := time.Now()
		physN, physErr := d.imgFile.ReadAt(
			data[n:],
			chunkOff,
		)
		physDur := time.Since(t1)

		if shmDur > 100*time.Millisecond || physDur > 100*time.Millisecond {
			log.Printf(
				"slow read chunk off=%d len=%d shm=%v phys=%v",
				chunkOff, len(chunk), shmDur, physDur,
			)
		}

		if physN > 0 {
			n += physN
		}

		if physErr != nil &&
			physErr != io.EOF {
			return fuse.ReadResultData(nil), toErrno(physErr)
		}

		if physN == 0 {
			/*
				Physical image also had nothing. Treat the rest
				as zeros so the caller sees a coherent block.
			*/
			break
		}
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
) (n uint32, errno syscall.Errno) {
	/*
		A panic inside the tracker, detector, or SHM layer must not
		kill the daemon: the STB would lose its block device
		mid-operation and its FAT driver would report the volume
		as corrupt. Convert panics to EIO so at least the daemon
		survives and subsequent writes can succeed.
	*/
	defer func() {
		if r := recover(); r != nil {
			log.Printf(
				"PANIC in Write off=%d len=%d: %v",
				off, len(data), r,
			)
			n = 0
			errno = syscall.EIO
		}
	}()

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

	/*
		The STB must never see EFBIG. If it writes past the
		advertised size, extend instead of refusing. The sparse
		image absorbs it; the alternative is the STB marking the
		volume as read-only.
	*/
	if cur := d.size.Load(); end > cur {
		log.Printf(
			"write extends past advertised size: off=%d len=%d old=%d new=%d",
			start, len(data), cur, end,
		)
		d.size.Store(end)
	}

	select {
	case <-ctx.Done():
		return 0, syscall.EINTR
	default:
	}

	d.logicalMu.Lock()
	defer d.logicalMu.Unlock()

	/*
		Accepting() is only false during teardown. Log it but do
		not return EIO: the inline fallback below can still
		serve the write, and an EIO here is indistinguishable
		from disk failure to the STB.
	*/
	if !d.writer.Accepting() {
		log.Printf(
			"write during writer shutdown off=%d len=%d (inline path)",
			start, len(data),
		)
	}

	/*
		Stage the whole FUSE write into /dev/shm.

		One generation covers the entire write; the physical
		plans below refer to subranges of that generation.

		If staging fails, fall back to writing straight through
		to the sparse image. The STB must see success; a slower
		path is acceptable, an EIO is not.
	*/
	staged, err := d.shm.StageWrite(data, start)
	if err != nil {
		log.Printf(
			"SHM stage failed off=%d len=%d: %v (falling back to direct write)",
			start, len(data), err,
		)

		if _, werr := d.imgFile.WriteAt(data, int64(start)); werr != nil {
			return 0, toErrno(werr)
		}

		return uint32(len(data)), 0
	}

	/*
		Classify the write.
	*/
	segs := d.tracker.Classify(start, uint64(len(data)))

	/*
		A tracker that returns nothing is a bug, not a normal
		condition. Default to RangeMeta so the bytes are
		persisted and the STB can read them back.
	*/
	if len(segs) == 0 {
		log.Printf(
			"BUG: tracker returned no ranges for write off=%d len=%d; treating as RangeMeta",
			start, len(data),
		)
		segs = []ByteRange{{
			Start: start,
			End:   end,
			Kind:  RangeMeta,
		}}
	}

	/*
		Clip tracker ranges to the write window rather than
		failing. A tracker with an off-by-one must not be able
		to take down the STB's filesystem.
	*/
	clipped := segs[:0]
	for _, seg := range segs {
		if seg.Start < start {
			seg.Start = start
		}
		if seg.End > end {
			seg.End = end
		}
		if seg.End <= seg.Start {
			continue
		}
		clipped = append(clipped, seg)
	}
	segs = clipped

	if len(segs) == 0 {
		/*
			Every segment clipped to zero length. Persist the
			whole write as RangeMeta. Same reasoning as above.
		*/
		segs = []ByteRange{{
			Start: start,
			End:   end,
			Kind:  RangeMeta,
		}}
	}

	if verbLog {
		log.Printf(
			"write off=%d len=%d segs=%d",
			start, len(data), len(segs),
		)
	}

	/*
		Build physical write plans for each segment.
	*/
	plans := make([]diskWritePlan, 0, len(segs))

	for _, seg := range segs {
		persist := true
		label := "DATA"

		if seg.Kind == RangeCandidate {
			persist = d.preserve
			label = "CANDIDATE"
		}

		plans = append(plans, diskWritePlan{
			start: seg.Start,
			end:   seg.End,

			persist: persist,

			overlay: []ShmExtent{{
				Start: seg.Start,
				End:   seg.End,
				Gen:   staged.Gen,
			}},

			label: label,
		})
	}

	/*
		Advance the logical filesystem / TS state immediately.
	*/
	var punches []ByteRange

	for _, seg := range segs {
		a := int(seg.Start - start)
		b := int(seg.End - start)

		part := data[a:b]

		switch seg.Kind {
		case RangeCandidate:
			streamID := seg.StreamID
			if streamID == "" {
				streamID = "default"
			}

			confirmed := d.feedCandidate(
				streamID,
				seg.StreamOffset,
				part,
			)

			/*
				FSTracker has no discovery phase. For NTFS the
				punch is produced later by DrainDiscovery; for
				FAT32 the detection is synchronous, so emit the
				punch here in the same operation.
			*/
			if !d.preserve && confirmed {
				punches = append(punches, ByteRange{
					Start: seg.Start,
					End:   seg.End,
					Kind:  seg.Kind,
				})
			}

		case RangeMeta, RangeNormal, RangeUnknown:
			newPunches, errno := d.processMetadataWrite(
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

			punches = append(punches, newPunches...)
		}
	}

	punches = coalescePunchRanges(punches)

	/*
		Promote any candidate that is not covered by a punch.

		The original design assumed every RangeCandidate was
		either punched (physical zeroed) or persisted. The
		detector guard and the FAT32 punch path above ensure
		that TS-confirmed candidates are punched. Anything left
		is a candidate that was not confirmed as MPEG-TS;
		persist it so read-after-write still works for the STB.
	*/
	for i := range plans {
		if plans[i].persist {
			continue
		}

		covered := false

		for _, p := range punches {
			if p.Start <= plans[i].start &&
				p.End >= plans[i].end {
				covered = true
				break
			}
		}

		if !covered {
			plans[i].persist = true
			plans[i].label = "CANDIDATE_PERSIST"

			if verbLog {
				log.Printf(
					"candidate not punched; persisting [%d,%d)",
					plans[i].start,
					plans[i].end,
				)
			}
		}
	}

	/*
		Build punch plans from the coalesced punch ranges.
	*/
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

	op := diskWriteOp{
		writes:  plans,
		punches: coalescePunchPlans(punchPlans),
	}

	/*
		Enqueue for asynchronous physical persistence.

		If the writer is shutting down, persist inline instead
		of returning an error the STB cannot recover from.
		Skip punches in that case: reclaiming space during
		teardown is not worth the risk.
	*/
	if err := d.writer.Enqueue(op); err != nil {
		log.Printf(
			"writer enqueue failed off=%d len=%d: %v (persisting inline)",
			start, len(data), err,
		)

		if _, werr := d.imgFile.WriteAt(data, int64(start)); werr != nil {
			_ = d.shm.ReleaseIfCurrent([]ShmExtent{staged})
			return 0, toErrno(werr)
		}

		_ = d.shm.ReleaseIfCurrent([]ShmExtent{staged})
		return uint32(len(data)), 0
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
		Insert the barrier while holding logicalMu so it is ordered
		after all logically submitted writes.

		IMPORTANT:
		Release logicalMu BEFORE waiting for physical persistence.

		Otherwise a slow disk blocks the logical filesystem again.
	*/
	d.logicalMu.Lock()

	done, err := d.writer.EnqueueSync()

	d.logicalMu.Unlock()

	if err != nil {
		return toErrno(err)
	}

	select {
	case err := <-done:
		delay := time.Since(start)

		if delay > 20*time.Millisecond {
			log.Printf(
				"slow queued sync took=%v",
				delay,
			)
		}

		return toErrno(err)

	case <-ctx.Done():
		return syscall.EINTR
	}
}
