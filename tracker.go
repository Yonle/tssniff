package main

import (
	"encoding/binary"
	"io"
)

type FSTracker struct {
	fsType      string
	partition   Partition
	metadataEnd uint64
	bytesPerSec uint32
}

func NewFSTracker(fsType string, part Partition, r io.ReaderAt) *FSTracker {
	tracker := &FSTracker{
		fsType:    fsType,
		partition: part,
	}
	tracker.initMetadataBoundaries(r)
	return tracker
}

func (t *FSTracker) initMetadataBoundaries(r io.ReaderAt) {
	if t.partition.Size == 0 {
		// No partition info; be conservative.
		t.metadataEnd = 2 * 1024 * 1024
		return
	}

	var boot [512]byte
	if _, err := r.ReadAt(boot[:], t.partition.Offset); err != nil {
		t.metadataEnd = uint64(t.partition.Offset) + (2 * 1024 * 1024)
		return
	}

	// Require the classic boot signature (0x55 0xAA at offset 510).
	// Without this we cannot trust the FAT size fields.
	if boot[510] != 0x55 || boot[511] != 0xAA {
		t.metadataEnd = uint64(t.partition.Offset) + (2 * 1024 * 1024)
		return
	}

	t.bytesPerSec = uint32(binary.LittleEndian.Uint16(boot[11:13]))
	if t.bytesPerSec == 0 {
		t.bytesPerSec = 512
	}

	reservedSectors := uint32(binary.LittleEndian.Uint16(boot[14:16]))
	numFATs := uint32(boot[16])
	fatSize := uint32(binary.LittleEndian.Uint32(boot[36:40]))

	partitionSectors := uint64(t.partition.Size) / uint64(t.bytesPerSec)

	var metaSectors uint64

	if fatSize > 0 && numFATs > 0 {
		// FAT32.  Trust the boot sector's FATSz32.
		metaSectors = uint64(reservedSectors) +
			uint64(numFATs)*uint64(fatSize) +
			32 // root dir clusters

		if metaSectors > partitionSectors {
			metaSectors = partitionSectors
		}
	} else {
		// exFAT or unknown: fall back to a fixed 4 MB window.
		metaSectors = (4 * 1024 * 1024) / uint64(t.bytesPerSec)
	}

	t.metadataEnd = uint64(t.partition.Offset) + metaSectors*uint64(t.bytesPerSec)

	// Metadata can never extend past the partition.
	partitionEnd := uint64(t.partition.Offset) + uint64(t.partition.Size)
	if t.metadataEnd > partitionEnd {
		t.metadataEnd = partitionEnd
	}
}

func (t *FSTracker) IsMetadata(off uint64, size uint32) bool {
	if off < 512 {
		return true
	}
	if off >= uint64(t.partition.Offset) && off < t.metadataEnd {
		return true
	}
	return false
}

// MetadataEnd returns the byte offset where metadata ends. Exposed for
// diagnostics.
func (t *FSTracker) MetadataEnd() uint64 {
	return t.metadataEnd
}