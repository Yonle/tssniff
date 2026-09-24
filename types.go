package main

type RangeKind uint8

const (
	RangeMeta RangeKind = iota
	RangeNormal
	RangeCandidate
	RangeUnknown
)

type ByteRange struct {
	Start uint64
	End   uint64
	Kind  RangeKind
	Name  string

	StreamID     string
	StreamOffset uint64
}
