package main

import (
	"log"
	"sync"
)

const captureHistoryCapacity = 65536

type CaptureJournal struct {
	items [captureHistoryCapacity]CaptureRange

	head  int
	count int
}

func (j *CaptureJournal) Add(
	c CaptureRange,
) {
	j.items[j.head] = c

	j.head =
		(j.head + 1) %
			len(j.items)

	if j.count <
		len(j.items) {
		j.count++
	}
}

func (j *CaptureJournal) ForOverlap(
	start,
	end uint64,
	fn func(CaptureRange),
) {
	if end <= start ||
		j.count == 0 {
		return
	}

	first :=
		(j.head -
			j.count +
			len(j.items)) %
			len(j.items)

	for i := 0; i < j.count; i++ {
		idx :=
			(first + i) %
				len(j.items)

		c := j.items[idx]

		if c.End <= start ||
			c.Start >= end {
			continue
		}

		fn(c)
	}
}

type Reconciler struct {
	in <-chan ObservedWrite

	ntfs *NTFS

	pipeline *WritePipeline

	captures CaptureJournal

	wg sync.WaitGroup
}

func NewReconciler(
	in <-chan ObservedWrite,
	ntfs *NTFS,
	pipeline *WritePipeline,
) *Reconciler {
	return &Reconciler{
		in: in,

		ntfs: ntfs,

		pipeline: pipeline,
	}
}

func (r *Reconciler) Start() {
	r.wg.Add(1)

	go r.run()
}

func (r *Reconciler) Wait() {
	r.wg.Wait()
}

func (r *Reconciler) run() {
	defer r.wg.Done()

	for ev := range r.in {
		/*
			Remember every MPEG-TS capture.

			If NTFS already knows this physical range, it can be
			punched immediately.

			Otherwise it remains in the journal until an MFT update
			reveals the corresponding file-data range.
		*/
		for _, capture := range ev.Captures {
			r.captures.Add(capture)

			r.reconcileCapture(capture)
		}

		if !ev.TouchesMFT {
			continue
		}

		/*
			Every MFT write is a reconciliation point.

			ObserveMFTWrite() refreshes the NTFS state using the
			acknowledged SHM contents.

			It returns only physical data ranges which were not
			already known. Those are the only ranges which need
			to be matched against historical captures.
		*/
		newRanges := r.ntfs.ObserveMFTWrite(
			ev.Offset,
			ev.End,
		)

		for _, dataRange := range newRanges {
			r.captures.ForOverlap(
				dataRange.Start,
				dataRange.End,
				func(c CaptureRange) {
					start := maxU64(
						c.Start,
						dataRange.Start,
					)

					end := minU64(
						c.End,
						dataRange.End,
					)

					if start >= end {
						return
					}

					r.submitPunch(
						c.Seq,
						start,
						end,
					)
				},
			)
		}
	}
}

func (r *Reconciler) reconcileCapture(
	capture CaptureRange,
) {
	for _, dataRange := range r.ntfs.DataRanges() {
		if dataRange.End <= capture.Start ||
			dataRange.Start >= capture.End {
			continue
		}

		start := maxU64(
			capture.Start,
			dataRange.Start,
		)

		end := minU64(
			capture.End,
			dataRange.End,
		)

		if start >= end {
			continue
		}

		r.submitPunch(
			capture.Seq,
			start,
			end,
		)
	}
}

func (r *Reconciler) submitPunch(
	seq,
	start,
	end uint64,
) {
	if start >= end {
		return
	}

	ok := r.pipeline.SubmitPunch(
		PunchRequest{
			CaptureSeq: seq,
			Start:      start,
			End:        end,
		},
	)

	if !ok && verbLog {
		log.Printf(
			"punch deferred captureSeq=%d phys=[%d,%d)",
			seq,
			start,
			end,
		)
	}
}
