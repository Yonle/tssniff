package main

type RangeKind int

const (
	RangeUnknown RangeKind = iota
	RangeMeta
	RangeData
)

type ByteRange struct {
	Start uint64
	End   uint64
	Kind  RangeKind
	Name  string
}
