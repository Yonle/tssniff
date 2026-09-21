package main

import (
	"encoding/binary"
	"fmt"
	"os"
)

const mbrSectorSize = 512

type Partition struct {
	Offset int64
	Size   int64
	Type   byte
}

func findMBRPartitions(f *os.File) ([]Partition, error) {
	var mbr [mbrSectorSize]byte

	if _, err := f.ReadAt(mbr[:], 0); err != nil {
		return nil, fmt.Errorf("read MBR: %w", err)
	}

	if binary.LittleEndian.Uint16(mbr[510:512]) != 0xAA55 {
		return nil, fmt.Errorf("invalid MBR signature")
	}

	partitions := make([]Partition, 0, 4)

	for i := 0; i < 4; i++ {
		off := 446 + i*16

		partType := mbr[off+4]
		if partType == 0 {
			continue
		}

		startLBA := binary.LittleEndian.Uint32(
			mbr[off+8 : off+12],
		)

		sectors := binary.LittleEndian.Uint32(
			mbr[off+12 : off+16],
		)

		if startLBA == 0 || sectors == 0 {
			continue
		}

		offset := int64(startLBA) * mbrSectorSize
		size := int64(sectors) * mbrSectorSize

		partitions = append(partitions, Partition{
			Offset: offset,
			Size:   size,
			Type:   partType,
		})
	}

	if len(partitions) == 0 {
		return nil, fmt.Errorf("no MBR partitions found")
	}

	return partitions, nil
}
