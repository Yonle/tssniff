package main

import (
	"context"
	"log"
	"os"
	"sync"
	"syscall"
	"time"
)

const (
	writerChunkSize = 1 << 20 // 1 MiB

	fallocKeepSize  uint32 = 0x01
	fallocPunchHole uint32 = 0x02
)

type diskWritePlan struct {
	start,
	end uint64

	persist bool

	/*
		Exact logical generations represented by this operation.

		The writer must NEVER blindly persist a newer generation.
	*/
	overlay []ShmExtent

	label string
}

type diskPunchPlan struct {
	start,
	end uint64

	/*
		Overlay generations which existed when the logical replay decided
		that this physical range should disappear.

		If newer writes replace them before the worker gets here,
		ReleaseIfCurrent() will leave those newer writes alone.
	*/
	overlay []ShmExtent
}

type diskWriteOp struct {
	writes  []diskWritePlan
	punches []diskPunchPlan

	barrier chan error
}

type DiskWriter struct {
	disk *os.File
	shm  *SHMDisk

	queueMu sync.Mutex
	queue   []diskWriteOp
	cond    *sync.Cond

	closing bool

	wg sync.WaitGroup

	errMu      sync.Mutex
	pendingErr error
}

func NewDiskWriter(
	disk *os.File,
	shm *SHMDisk,
) *DiskWriter {
	w := &DiskWriter{
		disk: disk,
		shm:  shm,
	}

	w.cond = sync.NewCond(
		&w.queueMu,
	)

	w.wg.Add(1)

	go w.worker()

	return w
}

func (w *DiskWriter) Accepting() bool {
	w.queueMu.Lock()
	defer w.queueMu.Unlock()

	return !w.closing
}

func (w *DiskWriter) Enqueue(
	op diskWriteOp,
) error {
	w.queueMu.Lock()
	defer w.queueMu.Unlock()

	if w.closing {
		return syscall.EIO
	}

	w.queue = append(
		w.queue,
		op,
	)

	w.cond.Signal()

	return nil
}

func (w *DiskWriter) EnqueueSync() (<-chan error, error) {
	done := make(chan error, 1)

	w.queueMu.Lock()
	defer w.queueMu.Unlock()

	if w.closing {
		return nil, syscall.EIO
	}

	w.queue = append(
		w.queue,
		diskWriteOp{
			barrier: done,
		},
	)

	w.cond.Signal()

	return done, nil
}

func (w *DiskWriter) Sync(
	ctx context.Context,
) error {
	done := make(chan error, 1)

	w.queueMu.Lock()

	if w.closing {
		w.queueMu.Unlock()

		return syscall.EIO
	}

	w.queue = append(
		w.queue,
		diskWriteOp{
			barrier: done,
		},
	)

	w.cond.Signal()

	w.queueMu.Unlock()

	select {
	case err := <-done:
		return err

	case <-ctx.Done():
		return ctx.Err()
	}
}

/*
Close stops accepting new work and drains the physical queue before exiting.

Call this only after FUSE is no longer submitting new writes.
*/
func (w *DiskWriter) Close() error {
	done := make(chan error, 1)

	w.queueMu.Lock()

	if w.closing {
		w.queueMu.Unlock()
		w.wg.Wait()

		return nil
	}

	w.closing = true

	w.queue = append(
		w.queue,
		diskWriteOp{
			barrier: done,
		},
	)

	w.cond.Signal()

	w.queueMu.Unlock()

	err := <-done

	w.wg.Wait()

	return err
}

func (w *DiskWriter) worker() {
	defer w.wg.Done()

	for {
		op, ok := w.pop()

		if !ok {
			return
		}

		if op.barrier != nil {
			err := w.disk.Sync()

			pending := w.takePendingError()

			if err == nil {
				err = pending
			}

			op.barrier <- err
			close(op.barrier)

			continue
		}

		err := w.process(
			op,
		)

		if err != nil {
			w.recordError(err)

			log.Printf(
				"physical writer operation failed: %v",
				err,
			)
		}
	}
}

func (w *DiskWriter) pop() (
	diskWriteOp,
	bool,
) {
	w.queueMu.Lock()
	defer w.queueMu.Unlock()

	for len(w.queue) == 0 &&
		!w.closing {
		w.cond.Wait()
	}

	if len(w.queue) == 0 &&
		w.closing {
		return diskWriteOp{}, false
	}

	op := w.queue[0]

	copy(
		w.queue,
		w.queue[1:],
	)

	w.queue[len(w.queue)-1] = diskWriteOp{}
	w.queue = w.queue[:len(w.queue)-1]

	return op, true
}

func (w *DiskWriter) process(
	op diskWriteOp,
) error {
	var (
		firstErr error

		release []ShmExtent
	)

	/*
		Process physical data writes first.

		The SHM overlay remains authoritative until the entire logical
		operation has completed.
	*/
	for _, plan := range op.writes {
		if plan.end <= plan.start {
			continue
		}

		if !plan.persist {
			/*
				Capture-only candidate.

				It does not need physical persistence. Once the whole
				operation finishes, its SHM generations may be released.
			*/
			release = append(
				release,
				w.shm.CurrentMatching(
					plan.overlay,
				)...,
			)

			continue
		}

		released, err := w.persistPlan(
			plan,
		)

		release = append(
			release,
			released...,
		)

		if err != nil &&
			firstErr == nil {
			firstErr = err
		}
	}

	/*
		Now perform deferred physical hole punching.

		This is deliberately after the writes belonging to this logical
		operation.

		That preserves:

		    write payload
		    discover MPEG-TS
		    punch temporary storage

		while ensuring that unrelated RangeNormal/RangeMeta writes never
		get punched merely because they happened to be nearby.
	*/
	for _, plan := range op.punches {
		if plan.end <= plan.start {
			continue
		}

		if errno := w.punchHole(
			plan.start,
			plan.end-plan.start,
		); errno != 0 {
			if firstErr == nil {
				firstErr = errno
			}

			continue
		}

		/*
			The physical base is now cleaned.

			Only release SHM generations which are still the same ones
			that existed when the punch was scheduled.
		*/
		release = append(
			release,
			plan.overlay...,
		)
	}

	/*
		Only now release the volatile overlay.

		If another logical write replaced part of the range while the
		physical worker was busy, ReleaseIfCurrent() leaves that newer
		generation alive.
	*/
	release = coalesceWriterExtents(release)

	if err := w.shm.ReleaseIfCurrent(
		release,
	); err != nil {
		log.Printf(
			"SHM release failed: %v",
			err,
		)

		if firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

func (w *DiskWriter) persistPlan(
	plan diskWritePlan,
) ([]ShmExtent, error) {
	matches := w.shm.CurrentMatching(
		plan.overlay,
	)

	if len(matches) == 0 {
		/*
			The whole range was replaced by newer writes.

			The newer physical operation owns those bytes now.
		*/
		return nil, nil
	}

	buf := make(
		[]byte,
		writerChunkSize,
	)

	released := make(
		[]ShmExtent,
		0,
		len(matches),
	)

	var firstErr error

	for _, e := range matches {
		pos := e.Start

		for pos < e.End {
			n := uint64(writerChunkSize)

			if n > e.End-pos {
				n = e.End - pos
			}

			chunk := ShmExtent{
				Start: pos,
				End:   pos + n,
				Gen:   e.Gen,
			}

			data := buf[:int(n)]

			/*
				Read only if the exact generation still owns the chunk.

				A newer write may have arrived while the physical disk
				was blocked.
			*/
			err := w.shm.ReadGeneration(
				data,
				chunk,
			)

			if errorsIsStale(err) {
				pos += n
				continue
			}

			if err != nil {
				if firstErr == nil {
					firstErr = err
				}

				pos += n
				continue
			}

			if errno := w.writeBacking(
				data,
				pos,
				plan.label,
			); errno != 0 {
				if firstErr == nil {
					firstErr = errno
				}

				/*
					Do NOT release this range.

					The SHM overlay must remain authoritative after a
					physical persistence failure.
				*/
				pos += n
				continue
			}

			released = append(
				released,
				chunk,
			)

			pos += n
		}
	}

	return released, firstErr
}

func errorsIsStale(
	err error,
) bool {
	return err == ErrSHMStale
}

func (w *DiskWriter) writeBacking(
	data []byte,
	off uint64,
	kind string,
) syscall.Errno {
	start := time.Now()

	n, err := w.disk.WriteAt(
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

func (w *DiskWriter) punchHole(
	off,
	length uint64,
) syscall.Errno {
	if length == 0 {
		return 0
	}

	err := syscall.Fallocate(
		int(w.disk.Fd()),
		fallocPunchHole|fallocKeepSize,
		int64(off),
		int64(length),
	)

	if err != nil {
		log.Printf(
			"punch hole failed off=%d len=%d: %v",
			off,
			length,
			err,
		)

		return toErrno(err)
	}

	return 0
}

func (w *DiskWriter) recordError(
	err error,
) {
	if err == nil {
		return
	}

	w.errMu.Lock()
	defer w.errMu.Unlock()

	if w.pendingErr == nil {
		w.pendingErr = err
	}
}

func (w *DiskWriter) takePendingError() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()

	err := w.pendingErr
	w.pendingErr = nil

	return err
}

func coalesceWriterExtents(
	in []ShmExtent,
) []ShmExtent {
	if len(in) == 0 {
		return nil
	}

	out := append(
		[]ShmExtent(nil),
		in...,
	)

	sortShmExtents(out)

	merged := make(
		[]ShmExtent,
		0,
		len(out),
	)

	for _, e := range out {
		if e.End <= e.Start {
			continue
		}

		if len(merged) > 0 {
			last := &merged[len(merged)-1]

			if last.End == e.Start &&
				last.Gen == e.Gen {
				last.End = e.End
				continue
			}
		}

		merged = append(
			merged,
			e,
		)
	}

	return merged
}
