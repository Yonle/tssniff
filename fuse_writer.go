package main

import (
	"context"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	writerChunkSize = 1 << 20 // 1 MiB

	fallocKeepSize  uint32 = 0x01
	fallocPunchHole uint32 = 0x02

	releaseBatch      = 32
	punchQueueSoftCap = 256
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

	/*
		Physical punches run on a separate goroutine so the
		FUSE reader is never blocked behind Fallocate on the
		sparse image. Unbounded slice with a soft cap; if the
		puncher falls behind, the cap forces inline execution.
	*/
	punchMu    sync.Mutex
	punchQueue []diskPunchPlan
	punchCond  *sync.Cond

	/*
		closing is set exactly once, by the first Close() caller.

		After it is true:
		  - Enqueue / EnqueueSync / Sync return EIO
		  - pop() and popPunch() exit once their queues drain

		It is read under queueMu in the writer path and under
		punchMu in the punch path, but stored via CAS so the
		"first closer" election does not need a lock.
	*/
	closing atomic.Bool

	wg      sync.WaitGroup
	punchWg sync.WaitGroup

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

	w.cond = sync.NewCond(&w.queueMu)
	w.punchCond = sync.NewCond(&w.punchMu)

	w.wg.Add(1)
	go w.worker()

	w.punchWg.Add(1)
	go w.punchWorker()

	return w
}

func (w *DiskWriter) Accepting() bool {
	return !w.closing.Load()
}

func (w *DiskWriter) Enqueue(
	op diskWriteOp,
) error {
	w.queueMu.Lock()
	defer w.queueMu.Unlock()

	if w.closing.Load() {
		return syscall.EIO
	}

	w.queue = append(w.queue, op)
	w.cond.Signal()

	return nil
}

func (w *DiskWriter) EnqueueSync() (<-chan error, error) {
	done := make(chan error, 1)

	w.queueMu.Lock()
	defer w.queueMu.Unlock()

	if w.closing.Load() {
		return nil, syscall.EIO
	}

	w.queue = append(w.queue, diskWriteOp{barrier: done})
	w.cond.Signal()

	return done, nil
}

func (w *DiskWriter) Sync(
	ctx context.Context,
) error {
	done := make(chan error, 1)

	w.queueMu.Lock()

	if w.closing.Load() {
		w.queueMu.Unlock()
		return syscall.EIO
	}

	w.queue = append(w.queue, diskWriteOp{barrier: done})
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
Close stops accepting new work and drains both the physical write queue
and the deferred punch queue before returning.

Call this only after FUSE is no longer submitting new writes.

The first caller elects itself via CompareAndSwap and performs the
drain. Subsequent callers wait for the same drain to finish.
*/
func (w *DiskWriter) Close() error {
	if !w.closing.CompareAndSwap(false, true) {
		/*
			Another Close is already in progress. Wait for it.

			The first closer will signal both cond variables and
			wait for both worker WaitGroups; waiting on the same
			WaitGroups here is sufficient.
		*/
		w.wg.Wait()
		w.punchWg.Wait()
		return nil
	}

	/*
		Enqueue a drain barrier after closing is set. Any Enqueue
		that wins queueMu first still lands before the barrier,
		because the barrier append also takes queueMu.

		Broadcast (not Signal) so a worker currently in Wait()
		wakes regardless of which cond it is parked on.
	*/
	done := make(chan error, 1)

	w.queueMu.Lock()
	w.queue = append(w.queue, diskWriteOp{barrier: done})
	w.cond.Broadcast()
	w.queueMu.Unlock()

	err := <-done

	w.wg.Wait()

	/*
		Wake the punch worker so it can exit after draining its
		own queue.
	*/
	w.punchMu.Lock()
	w.punchCond.Broadcast()
	w.punchMu.Unlock()

	w.punchWg.Wait()

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

		err := w.process(op)
		if err != nil {
			w.recordError(err)
			log.Printf("physical writer operation failed: %v", err)
		}
	}
}

func (w *DiskWriter) pop() (
	diskWriteOp,
	bool,
) {
	w.queueMu.Lock()
	defer w.queueMu.Unlock()

	for len(w.queue) == 0 && !w.closing.Load() {
		w.cond.Wait()
	}

	if len(w.queue) == 0 && w.closing.Load() {
		return diskWriteOp{}, false
	}

	op := w.queue[0]

	copy(w.queue, w.queue[1:])
	w.queue[len(w.queue)-1] = diskWriteOp{}
	w.queue = w.queue[:len(w.queue)-1]

	return op, true
}

func (w *DiskWriter) process(
	op diskWriteOp,
) error {
	var (
		firstErr error
		release  []ShmExtent
	)

	for _, plan := range op.writes {
		if plan.end <= plan.start {
			continue
		}

		if !plan.persist {
			/*
				Capture-only candidate. Its overlay is released
				after the punch worker has confirmed the physical
				punch, so the read path keeps seeing the staged
				bytes until the physical range is actually zeroed.
			*/
			continue
		}

		released, err := w.persistPlan(plan)
		release = append(release, released...)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	/*
		Hand punches to the background worker. The overlay for
		these ranges is released by the punch worker after the
		Fallocate succeeds, not here.
	*/
	for _, plan := range op.punches {
		if plan.end <= plan.start {
			continue
		}

		if overlapsPersisted(plan.start, plan.end, op.writes) {
			log.Printf(
				"skip punch [%d,%d): overlaps persisted write in same op",
				plan.start, plan.end,
			)
			/*
				The candidate was persisted, so its SHM overlay
				belongs to the persist path. Release now.
			*/
			release = append(release, plan.overlay...)
			continue
		}

		w.enqueuePunch(plan)
	}

	/*
		Batch releases so a large op does not hold the SHM write
		lock across an unbounded number of Fallocate calls.
	*/
	release = coalesceWriterExtents(release)

	for i := 0; i < len(release); i += releaseBatch {
		j := i + releaseBatch
		if j > len(release) {
			j = len(release)
		}

		if err := w.shm.ReleaseIfCurrent(release[i:j]); err != nil {
			log.Printf("SHM release failed: %v", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

func (w *DiskWriter) persistPlan(
	plan diskWritePlan,
) ([]ShmExtent, error) {
	matches := w.shm.CurrentMatching(plan.overlay)

	if len(matches) == 0 {
		/*
			The whole range was replaced by newer writes.

			The newer physical operation owns those bytes now.
		*/
		return nil, nil
	}

	buf := make([]byte, writerChunkSize)

	released := make([]ShmExtent, 0, len(matches))

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
			err := w.shm.ReadGeneration(data, chunk)

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

			if errno := w.writeBacking(data, pos, plan.label); errno != 0 {
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

			released = append(released, chunk)
			pos += n
		}
	}

	return released, firstErr
}

func (w *DiskWriter) enqueuePunch(plan diskPunchPlan) {
	w.punchMu.Lock()

	if w.closing.Load() {
		w.punchMu.Unlock()
		return
	}

	w.punchQueue = append(w.punchQueue, plan)
	w.punchCond.Signal()

	/*
		Soft cap. If the puncher cannot keep up, the writer
		executes the punch inline. That reintroduces the
		latency we were trying to avoid, but only under
		sustained overload, and it is bounded — the queue
		does not grow without limit.
	*/
	overflow := len(w.punchQueue) > punchQueueSoftCap

	w.punchMu.Unlock()

	if overflow {
		log.Printf("punch queue overflow (%d), punching inline", len(w.punchQueue))

		if err := w.punchInline(plan); err != nil {
			log.Printf("inline punch failed [%d,%d): %v", plan.start, plan.end, err)
		}
	}
}

func (w *DiskWriter) punchWorker() {
	defer w.punchWg.Done()

	for {
		plan, ok := w.popPunch()
		if !ok {
			return
		}

		if err := w.punchInline(plan); err != nil {
			log.Printf(
				"async punch failed [%d,%d): %v",
				plan.start, plan.end, err,
			)
		}
	}
}

func (w *DiskWriter) popPunch() (diskPunchPlan, bool) {
	w.punchMu.Lock()
	defer w.punchMu.Unlock()

	for len(w.punchQueue) == 0 && !w.closing.Load() {
		w.punchCond.Wait()
	}

	if len(w.punchQueue) == 0 && w.closing.Load() {
		return diskPunchPlan{}, false
	}

	plan := w.punchQueue[0]

	copy(w.punchQueue, w.punchQueue[1:])
	w.punchQueue[len(w.punchQueue)-1] = diskPunchPlan{}
	w.punchQueue = w.punchQueue[:len(w.punchQueue)-1]

	return plan, true
}

func (w *DiskWriter) punchInline(plan diskPunchPlan) error {
	if errno := w.punchHole(plan.start, plan.end-plan.start); errno != 0 {
		return errno
	}

	/*
		Physical range is now zeroed. Release the SHM overlay
		generations that existed when the punch was scheduled.

		ReleaseIfCurrent will leave newer generations alone.
	*/
	if err := w.shm.ReleaseIfCurrent(plan.overlay); err != nil {
		return err
	}

	return nil
}

func errorsIsStale(err error) bool {
	return err == ErrSHMStale
}

func overlapsPersisted(
	start,
	end uint64,
	writes []diskWritePlan,
) bool {
	for _, p := range writes {
		if !p.persist || p.end <= p.start {
			continue
		}

		if start < p.end && p.start < end {
			return true
		}
	}

	return false
}

func (w *DiskWriter) writeBacking(
	data []byte,
	off uint64,
	kind string,
) syscall.Errno {
	start := time.Now()

	n, err := w.disk.WriteAt(data, int64(off))

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
			off, length, err,
		)

		return toErrno(err)
	}

	return 0
}

func (w *DiskWriter) recordError(err error) {
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

func coalesceWriterExtents(in []ShmExtent) []ShmExtent {
	if len(in) == 0 {
		return nil
	}

	out := append([]ShmExtent(nil), in...)

	sortShmExtents(out)

	merged := make([]ShmExtent, 0, len(out))

	for _, e := range out {
		if e.End <= e.Start {
			continue
		}

		if len(merged) > 0 {
			last := &merged[len(merged)-1]

			if last.End == e.Start && last.Gen == e.Gen {
				last.End = e.End
				continue
			}
		}

		merged = append(merged, e)
	}

	return merged
}