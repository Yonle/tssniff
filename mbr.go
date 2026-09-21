package main

import (
	"encoding/binary"
	"fmt"
	"os"
)

const sectorSize = 512

func findMBRExFATPartition(f *os.File) (Partition, error) {
	var mbr [512]byte

	if _, err := f.ReadAt(mbr[:], 0); err != nil {
		return Partition{}, fmt.Errorf(
			"read MBR: %w",
			err,
		)
	}

	if binary.LittleEndian.Uint16(
		mbr[510:512],
	) != 0xAA55 {
		return Partition{}, fmt.Errorf(
			"invalid MBR signature",
		)
	}

	for i := 0; i < 4; i++ {
		off := 446 + i*16

		partType := mbr[off+4]

		if partType != 0x07 {
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

		partitionOffset :=
			int64(startLBA) * 512

		var boot [512]byte

		if _, err := f.ReadAt(
			boot[:],
			partitionOffset,
		); err != nil {
			continue
		}

		if string(boot[3:11]) != "EXFAT   " {
			continue
		}

		return Partition{
			Offset: partitionOffset,
			Size:   int64(sectors) * 512,
			Type:   partType,
		}, nil
	}

	return Partition{}, fmt.Errorf(
		"no exFAT partition found in MBR",
	)
}
