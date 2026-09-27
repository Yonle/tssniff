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
) bool {
	if len(data) == 0 {
		return false
	}

	d.tsMu.Lock()
	defer d.tsMu.Unlock()

	return d.feedCandidateLocked(streamID, logicalOffset, data)
}

func (d *DiskNode) feedCandidateLocked(
	streamID string,
	logicalOffset uint64,
	data []byte,
) bool {
	if len(data) == 0 {
		return false
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

		return false
	}

	if d.activeTSStream != streamID {
		d.detector.Reset()
		d.activeTSStream = streamID

		if verbLog {
			log.Printf("TS pipeline: switch stream=%q", streamID)
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

	return d.detector.detected && d.activeTSStream == streamID
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

	ordered := append([]ByteRange(nil), ranges...)

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

	replayed := make([]bool, len(ordered))

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

	/*
		Try new recordings first so a fresh recording can claim the
		active TS stream slot over an older one.

		We no longer blind-punch the ranges of a "new" stream. Any
		punch now only comes from replayCandidateRangeLocked(), which
		requires an actual MPEG-TS detection for that stream.
	*/
	for _, streamID := range newIDs {
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

		for i := range ordered {
			r := ordered[i]

			if r.StreamID != streamID {
				continue
			}

			if replayed[i] {
				continue
			}

			replayed[i] = true

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

	for i := range ordered {
		if replayed[i] {
			continue
		}

		r := ordered[i]

		streamID := r.StreamID

		if streamID == "" {
			streamID = "default"
		}

		/*
			Once a TS stream is active, do not replay unrelated
			streams. The previous code punched these ranges blindly,
			which is exactly what zeroed non-MPEG-TS data (FAT32
			FSINFO / directory clusters).
		*/
		if d.detector.detected &&
			d.activeTSStream != "" &&
			streamID != d.activeTSStream {

			if verbLog {
				log.Printf(
					"TS pipeline: skip non-active stream=%q active=%q range=[%d,%d)",
					streamID,
					d.activeTSStream,
					r.Start,
					r.End,
				)
			}

			continue
		}

		replayed[i] = true

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
		Only a range whose exact stream has been confirmed as MPEG-TS
		may be punched.

		This is the primary defense against zeroing FAT32 metadata
		(FSINFO sector 1, directory clusters, FAT tables) when the
		underlying image lives on a FAT32 partition.
	*/
	if !d.detector.detected ||
		d.activeTSStream != streamID {

		return nil, 0
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

func punchCovers(
	p diskWritePlan,
	punches []diskPunchPlan,
) bool {
	for _, q := range punches {
		if q.start <= p.start && q.end >= p.end {
			return true
		}
	}
	return false
}
