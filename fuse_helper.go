package main

import (
	"io"
	"log"
	"sort"
	"syscall"
)

func (d *DiskNode) feedCandidate(
	streamID string,
	logicalOffset uint64,
	data []byte,
) {
	if len(data) == 0 {
		return
	}

	d.tsMu.Lock()
	defer d.tsMu.Unlock()

	d.feedCandidateLocked(
		streamID,
		logicalOffset,
		data,
	)
}

func (d *DiskNode) feedCandidateLocked(
	streamID string,
	logicalOffset uint64,
	data []byte,
) {
	if len(data) == 0 {
		return
	}

	if streamID == "" {
		streamID = "default"
	}

	if d.detector.detected &&
		d.activeTSStream != "" &&
		streamID != d.activeTSStream {
		if verbLog {
			log.Printf(
				"TS pipeline: ignore stream=%q active=%q logical=%d",
				streamID,
				d.activeTSStream,
				logicalOffset,
			)
		}

		return
	}

	if d.activeTSStream != streamID {
		d.detector.Reset()
		d.activeTSStream = streamID

		if verbLog {
			log.Printf(
				"TS pipeline: switch stream=%q",
				streamID,
			)
		}
	}

	d.detector.Feed(
		streamID,
		logicalOffset,
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

	if d.detector.detected {
		d.activeTSStream = streamID
	}
}

func (d *DiskNode) processMetadataWrite(
	start,
	length uint64,
) ([]ByteRange, syscall.Errno) {
	d.tsMu.Lock()
	defer d.tsMu.Unlock()

	if d.tracker.InRootDir(start) {
		if verbLog {
			log.Printf(
				"root directory write at %d: reset TS detector",
				start,
			)
		}

		d.detector.Reset()
		d.activeTSStream = ""
	}

	d.tracker.OnMetadataWrite(
		start,
		length,
	)

	ntfs, ok := d.tracker.(*NTFSTracker)
	if !ok {
		return nil, 0
	}

	ranges, newStreams := ntfs.DrainDiscovery()

	if len(ranges) == 0 {
		return nil, 0
	}

	return d.replayCandidateRangesLocked(
		ranges,
		newStreams,
	)
}

func (d *DiskNode) replayCandidateRangesLocked(
	ranges []ByteRange,
	newStreams map[string]struct{},
) ([]ByteRange, syscall.Errno) {
	if len(ranges) == 0 {
		return nil, 0
	}

	ordered := append(
		[]ByteRange(nil),
		ranges...,
	)

	sort.Slice(
		ordered,
		func(i, j int) bool {
			if ordered[i].StreamID != ordered[j].StreamID {
				return ordered[i].StreamID <
					ordered[j].StreamID
			}

			if ordered[i].StreamOffset != ordered[j].StreamOffset {
				return ordered[i].StreamOffset <
					ordered[j].StreamOffset
			}

			return ordered[i].Start <
				ordered[j].Start
		},
	)

	var punches []ByteRange

	newIDs := make(
		[]string,
		0,
		len(newStreams),
	)

	for streamID := range newStreams {
		newIDs = append(
			newIDs,
			streamID,
		)
	}

	sort.Strings(newIDs)

	processedNew := make(
		map[string]struct{},
		len(newIDs),
	)

	for _, streamID := range newIDs {
		processedNew[streamID] = struct{}{}

		oldStream := d.activeTSStream

		d.detector.Reset()
		d.activeTSStream = streamID

		if verbLog {
			log.Printf(
				"TS pipeline: new recording stream=%q replacing=%q",
				streamID,
				oldStream,
			)
		}

		for _, r := range ordered {
			if r.StreamID != streamID {
				continue
			}

			newPunches, errno :=
				d.replayCandidateRangeLocked(r)

			if errno != 0 {
				return punches, errno
			}

			punches = append(
				punches,
				newPunches...,
			)
		}

		if d.detector.detected {
			break
		}
	}

	for _, r := range ordered {
		streamID := r.StreamID

		if streamID == "" {
			streamID = "default"
		}

		if _, isNew := newStreams[streamID]; isNew {
			if _, alreadyProcessed :=
				processedNew[streamID]; alreadyProcessed {

				if !d.preserve {
					punches = append(
						punches,
						ByteRange{
							Start: r.Start,
							End:   r.End,
							Kind:  r.Kind,
						},
					)
				}
			}

			continue
		}

		if d.detector.detected &&
			d.activeTSStream != "" &&
			streamID != d.activeTSStream {

			if !d.preserve {
				punches = append(
					punches,
					ByteRange{
						Start: r.Start,
						End:   r.End,
						Kind:  r.Kind,
					},
				)
			}

			continue
		}

		newPunches, errno :=
			d.replayCandidateRangeLocked(r)

		if errno != 0 {
			return punches, errno
		}

		punches = append(
			punches,
			newPunches...,
		)
	}

	return coalescePunchRanges(punches), 0
}

func (d *DiskNode) replayCandidateRangeLocked(
	r ByteRange,
) ([]ByteRange, syscall.Errno) {
	if verbLog {
		log.Printf(
			"NTFS replay candidate stream=%q logical=%d phys=[%d,%d)",
			r.StreamID,
			r.StreamOffset,
			r.Start,
			r.End,
		)
	}

	if r.End <= r.Start {
		return nil, 0
	}

	if r.Kind != RangeCandidate {
		return nil, 0
	}

	streamID := r.StreamID

	if streamID == "" {
		streamID = "default"
	}

	const replayChunkSize = 1 << 20

	buf := make(
		[]byte,
		replayChunkSize,
	)

	remaining := r.End - r.Start
	phys := r.Start
	logical := r.StreamOffset

	for remaining > 0 {
		n := uint64(replayChunkSize)

		if n > remaining {
			n = remaining
		}

		chunk := buf[:int(n)]

		readN, err := d.shm.ReadAt(
			chunk,
			int64(phys),
		)

		if err != nil &&
			err != io.EOF {
			log.Printf(
				"NTFS replay read failed phys=%d len=%d: %v",
				phys,
				n,
				err,
			)

			return nil, toErrno(err)
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

		d.feedCandidateLocked(
			streamID,
			logical,
			chunk[:readN],
		)

		phys += uint64(readN)
		logical += uint64(readN)
		remaining -= uint64(readN)

		if readN < int(n) {
			break
		}
	}

	/*
		Only replayed MPEG-TS candidates can generate punches.
	*/
	if !d.preserve {
		return []ByteRange{
			{
				Start: r.Start,
				End:   r.End,
				Kind:  r.Kind,
			},
		}, 0
	}

	return nil, 0
}

func coalescePunchRanges(
	in []ByteRange,
) []ByteRange {
	if len(in) == 0 {
		return nil
	}

	out := append(
		[]ByteRange(nil),
		in...,
	)

	sort.Slice(
		out,
		func(i, j int) bool {
			if out[i].Start != out[j].Start {
				return out[i].Start < out[j].Start
			}

			return out[i].End < out[j].End
		},
	)

	merged := make(
		[]ByteRange,
		0,
		len(out),
	)

	for _, r := range out {
		if r.End <= r.Start {
			continue
		}

		if len(merged) > 0 {
			last := &merged[len(merged)-1]

			if r.Start <= last.End {
				if r.End > last.End {
					last.End = r.End
				}

				continue
			}
		}

		merged = append(
			merged,
			r,
		)
	}

	return merged
}

func coalescePunchPlans(
	in []diskPunchPlan,
) []diskPunchPlan {
	if len(in) <= 1 {
		return in
	}

	sort.Slice(
		in,
		func(i, j int) bool {
			if in[i].start != in[j].start {
				return in[i].start < in[j].start
			}

			return in[i].end < in[j].end
		},
	)

	out := make(
		[]diskPunchPlan,
		0,
		len(in),
	)

	for _, p := range in {
		if p.end <= p.start {
			continue
		}

		if len(out) > 0 {
			last := &out[len(out)-1]

			if p.start <= last.end {
				if p.end > last.end {
					last.end = p.end
				}

				last.overlay = append(
					last.overlay,
					p.overlay...,
				)

				continue
			}
		}

		out = append(
			out,
			p,
		)
	}

	return out
}
