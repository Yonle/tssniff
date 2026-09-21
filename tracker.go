package main

type Tracker interface {
	Classify(start, length uint64) []ByteRange
	Snapshot() []ByteRange
	OverlapsMetadata(start, length uint64) bool
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

func coalesceRanges(
	in []ByteRange,
) []ByteRange {
	if len(in) == 0 {
		return nil
	}

	out := make(
		[]ByteRange,
		0,
		len(in),
	)

	for _, r := range in {
		if r.End <= r.Start {
			continue
		}

		if len(out) > 0 &&
			out[len(out)-1].End >= r.Start &&
			out[len(out)-1].Kind == r.Kind &&
			out[len(out)-1].Name == r.Name {

			if r.End > out[len(out)-1].End {
				out[len(out)-1].End = r.End
			}

			continue
		}

		out = append(out, r)
	}

	return out
}

func classifyRanges(
	meta,
	rules []ByteRange,
	start,
	length uint64,
) []ByteRange {
	if length == 0 {
		return nil
	}

	end := start + length

	bounds := []uint64{start, end}

	addBounds := func(r ByteRange) {
		if r.End <= start || r.Start >= end {
			return
		}

		if r.Start > start && r.Start < end {
			bounds = append(bounds, r.Start)
		}

		if r.End > start && r.End < end {
			bounds = append(bounds, r.End)
		}
	}

	for _, r := range meta {
		addBounds(r)
	}

	for _, r := range rules {
		addBounds(r)
	}

	sortUint64s(bounds)

	uniq := bounds[:0]

	for _, b := range bounds {
		if len(uniq) == 0 ||
			uniq[len(uniq)-1] != b {
			uniq = append(uniq, b)
		}
	}

	out := make(
		[]ByteRange,
		0,
		len(uniq)-1,
	)

	for i := 0; i+1 < len(uniq); i++ {
		a := uniq[i]
		b := uniq[i+1]

		if a == b {
			continue
		}

		kind := RangeUnknown
		name := ""

		// Metadata gets priority.
		for _, r := range meta {
			if r.Start <= a && b <= r.End {
				kind = RangeMeta
				name = r.Name
				break
			}
		}

		if kind == RangeUnknown {
			for _, r := range rules {
				if r.Start <= a && b <= r.End {
					kind = r.Kind
					name = r.Name
					break
				}
			}
		}

		out = append(
			out,
			ByteRange{
				Start: a,
				End:   b,
				Kind:  kind,
				Name:  name,
			},
		)
	}

	return coalesceRanges(out)
}

func sortUint64s(v []uint64) {
	// Tiny local helper so tracker implementations don't need to
	// know how classification is implemented.
	for i := 1; i < len(v); i++ {
		x := v[i]
		j := i - 1

		for j >= 0 && v[j] > x {
			v[j+1] = v[j]
			j--
		}

		v[j+1] = x
	}
}
