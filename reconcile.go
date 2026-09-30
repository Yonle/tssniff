package main

import "sync"

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
			Every MPEG-TS capture is remembered first.

			This is what allows a future MFT update to say:
			    "that video you saw 800 ms ago belongs to a file."
		*/
		for _, capture := range ev.Captures {
			r.captures.Add(capture)

			/*
				If NTFS already knows the area is user file data,
				punch can be scheduled immediately.
			*/
			for _, dataRange := range rangeOverlaps(
				capture.Start,
				capture.End,
				r.ntfs.DataRanges(),
			) {
				r.pipeline.SubmitPunch(
					PunchRequest{
						CaptureSeq: capture.Seq,

						Start: maxU64(
							capture.Start,
							dataRange.Start,
						),

						End: minU64(
							capture.End,
							dataRange.End,
						),
					},
				)
			}
		}

		if !ev.TouchesMFT {
			continue
		}

		/*
			The MFT write has now been fully observed by the sniffer.

			NTFS reads through SHM, so even if the writer has not yet
			persisted the metadata, NTFS sees the acknowledged bytes.
		*/
		newRanges := r.ntfs.ObserveMFTWrite(
			ev.Offset,
			ev.End,
		)

		/*
			These physical data ranges were not known previously.

			Intersect them with the historical MPEG-TS captures.
		*/
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

					r.pipeline.SubmitPunch(
						PunchRequest{
							CaptureSeq: c.Seq,
							Start:      start,
							End:        end,
						},
					)
				},
			)
		}
	}
}

func rangeOverlaps(
	start,
	end uint64,
	ranges []ByteRange,
) []ByteRange {
	if end <= start ||
		len(ranges) == 0 {
		return nil
	}

	var out []ByteRange

	for _, r := range ranges {
		if r.End <= start ||
			r.Start >= end {
			continue
		}

		out = append(
			out,
			ByteRange{
				Start: maxU64(
					start,
					r.Start,
				),
				End: minU64(
					end,
					r.End,
				),
			},
		)
	}

	return out
}
