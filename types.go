package main

type ByteRange struct {
	Start uint64
	End   uint64
}

type CaptureRange struct {
	Seq   uint64
	Start uint64
	End   uint64
}

type WriteEvent struct {
	Seq uint64

	Offset uint64
	Data   []byte

	/*
		Exact SHM generation representing this write.
	*/
	Staged ShmExtent

	/*
		Snapshot taken by FUSE Write().

		This tells the reconciler whether this write touched
		the currently-known physical MFT mapping.
	*/
	TouchesMFT bool
}

type ObservedWrite struct {
	Seq uint64

	Offset uint64
	End    uint64

	TouchesMFT bool

	Captures []CaptureRange
}

type PunchRequest struct {
	/*
		The capture which caused this punch.

		The writer uses this to prevent a newer physical write
		from being destroyed by a delayed punch.
	*/
	CaptureSeq uint64

	Start uint64
	End   uint64
}
