package main

import (
	"encoding/binary"
	"io"
)

type Tracker interface {
	Classify(start, length uint64) []ByteRange
	MetadataEnd() uint64
	InRootDir(off uint64) bool
	OnMetadataWrite(start, length uint64) []ByteRange
}

type FSTracker struct {
	fsType      string
	partition   Partition
	metadataEnd uint64
	dataStart   uint64
	rootStart   uint64
	rootEnd     uint64
	bytesPerSec uint32
}

func NewFSTracker(fsType string, part Partition, r io.ReaderAt) *FSTracker {
	t := &FSTracker{fsType: fsType, partition: part}
	t.initMetadataBoundaries(r)
	return t
}

func (t *FSTracker) initMetadataBoundaries(r io.ReaderAt) {
	if t.partition.Size == 0 {
		t.metadataEnd = 2 * 1024 * 1024
		return
	}

	var boot [512]byte
	if _, err := r.ReadAt(boot[:], t.partition.Offset); err != nil {
		t.metadataEnd = uint64(t.partition.Offset) + (2 * 1024 * 1024)
		return
	}
	if boot[510] != 0x55 || boot[511] != 0xAA {
		t.metadataEnd = uint64(t.partition.Offset) + (2 * 1024 * 1024)
		return
	}

	bps := uint32(binary.LittleEndian.Uint16(boot[11:13]))
	if bps == 0 {
		bps = 512
	}
	t.bytesPerSec = bps

	spc := uint32(boot[13])
	if spc == 0 {
		spc = 1
	}

	rsvd := uint32(binary.LittleEndian.Uint16(boot[14:16]))
	nfat := uint32(boot[16])
	fsz := uint32(binary.LittleEndian.Uint32(boot[36:40]))

	partSectors := uint64(t.partition.Size) / uint64(bps)

	var metaSectors uint64
	if fsz > 0 && nfat > 0 {
		// reserved + numFATs*FAT size + root dir (one cluster)
		metaSectors = uint64(rsvd) + uint64(nfat)*uint64(fsz) + uint64(spc)
		if metaSectors > partSectors {
			metaSectors = partSectors
		}
	} else {
		metaSectors = (4 * 1024 * 1024) / uint64(bps)
	}

	t.metadataEnd = uint64(t.partition.Offset) + metaSectors*uint64(bps)

	partEnd := uint64(t.partition.Offset) + uint64(t.partition.Size)
	if t.metadataEnd > partEnd {
		t.metadataEnd = partEnd
	}

	// dataStart = first byte of the data region (cluster 2).
	// The root directory lives there for FAT32.
	t.dataStart = uint64(t.partition.Offset) +
		(uint64(rsvd)+uint64(nfat)*uint64(fsz))*uint64(bps)

	clusterSize := uint64(spc) * uint64(bps)

	if fsz > 0 && nfat > 0 {
		rootCluster := binary.LittleEndian.Uint32(boot[44:48])
		if rootCluster < 2 {
			rootCluster = 2
		}
		t.rootStart = t.dataStart + (uint64(rootCluster)-2)*clusterSize
		t.rootEnd = t.rootStart + clusterSize
	} else {
		// Non-FAT32: treat the first 32 KB of the data region as
		// the directory area.
		t.rootStart = t.dataStart
		t.rootEnd = t.dataStart + 32*1024
	}
}

func (t *FSTracker) MetadataEnd() uint64 {
	return t.metadataEnd
}

func (t *FSTracker) InRootDir(off uint64) bool {
	return off >= t.rootStart && off < t.rootEnd
}

func (t *FSTracker) Classify(start, length uint64) []ByteRange {
	if length == 0 {
		return nil
	}
	end := start + length
	if start >= t.metadataEnd {
		return []ByteRange{{
			Start:        start,
			End:          end,
			Kind:         RangeCandidate,
			StreamID:     "fat32",
			StreamOffset: start,
		}}
	}
	if end <= t.metadataEnd {
		return []ByteRange{{Start: start, End: end, Kind: RangeMeta}}
	}
	return []ByteRange{
		{
			Start: start,
			End:   t.metadataEnd,
			Kind:  RangeMeta,
		},
		{
			Start:        t.metadataEnd,
			End:          end,
			Kind:         RangeCandidate,
			StreamID:     "fat32",
			StreamOffset: t.metadataEnd,
		},
	}
}

func (t *FSTracker) OnMetadataWrite(start, length uint64) []ByteRange {
	return nil
}
