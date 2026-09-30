package main

import (
	"encoding/binary"
	"fmt"
	"os"
)

const mbrSectorSize = 512

func findMBRPartition(
	f *os.File,
) (Partition, error) {
	var mbr [mbrSectorSize]byte

	if _, err := f.ReadAt(
		mbr[:],
		0,
	); err != nil {
		return Partition{},
			fmt.Errorf(
				"read MBR: %w",
				err,
			)
	}

	if binary.LittleEndian.Uint16(
		mbr[510:512],
	) != 0xAA55 {
		return Partition{},
			fmt.Errorf(
				"invalid MBR signature",
			)
	}

	for i := 0; i < 4; i++ {
		off :=
			446 +
				i*16

		partType := mbr[off+4]

		/*
			NTFS normally uses type 0x07.
		*/
		if partType != 0x07 {
			continue
		}

		startLBA := binary.LittleEndian.Uint32(
			mbr[off+8 : off+12],
		)

		sectors := binary.LittleEndian.Uint32(
			mbr[off+12 : off+16],
		)

		if startLBA == 0 ||
			sectors == 0 {
			continue
		}

		partitionOffset :=
			int64(startLBA) *
				mbrSectorSize

		return Partition{
			Offset: partitionOffset,

			Size: int64(sectors) *
				mbrSectorSize,

			Type: partType,
		}, nil
	}

	return Partition{},
		fmt.Errorf(
			"no NTFS partition found",
		)
}
