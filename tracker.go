package main

import (
	"sort"
	"sync/atomic"
)

type Tracker interface {
	Classify(start, length uint64) []ByteRange
	TSRanges() []ByteRange
	Refresh() error
}

type RangeKind uint8

const (
	RangeMeta RangeKind = iota
	RangeNormal
	RangeTS
	RangeUnknown
)

type ByteRange struct {
	Start uint64
	End   uint64 // exclusive

	Kind RangeKind
	Name string
}

type trackerSnapshot struct {
	meta       []ByteRange
	ts         []ByteRange
	classified []ByteRange
}

type rangeStore struct {
	state atomic.Pointer[trackerSnapshot]
}

func newRangeStore() *rangeStore {
	s := &rangeStore{}
	s.state.Store(&trackerSnapshot{})
	return s
}

func (s *rangeStore) set(meta, rules []ByteRange) {
	ts := make([]ByteRange, 0, 4)
	for _, r := range rules {
		if r.Kind == RangeTS {
			ts = append(ts, r)
		}
	}

	s.state.Store(&trackerSnapshot{
		meta:       meta,
		ts:         ts,
		classified: buildClassification(meta, rules),
	})
}

func (s *rangeStore) Classify(start, length uint64) []ByteRange {
	if length == 0 {
		return nil
	}

	end := start + length
	ranges := s.state.Load().classified
	out := make([]ByteRange, 0, 4)

	i := sort.Search(len(ranges), func(i int) bool {
		return ranges[i].End > start
	})

	pos := start
	for i < len(ranges) && pos < end {
		r := ranges[i]

		if r.Start > pos {
			gapEnd := minU64(r.Start, end)
			out = append(out, ByteRange{
				Start: pos,
				End:   gapEnd,
				Kind:  RangeUnknown,
			})
			pos = gapEnd
			if pos == end {
				break
			}
		}

		if r.End <= pos {
			i++
			continue
		}

		b := minU64(r.End, end)
		out = append(out, ByteRange{
			Start: pos,
			End:   b,
			Kind:  r.Kind,
			Name:  r.Name,
		})
		pos = b
		i++
	}

	if pos < end {
		out = append(out, ByteRange{
			Start: pos,
			End:   end,
			Kind:  RangeUnknown,
		})
	}

	return out
}

func (s *rangeStore) TSRanges() []ByteRange {
	return s.state.Load().ts
}
