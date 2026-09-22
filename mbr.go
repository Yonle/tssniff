package main

import (
	"encoding/binary"
	"fmt"
	"os"
)

const sectorSize = 512

type mbrFilesystem struct {
	partTypes   []byte
	signatureAt int
	signature   string
}

var mbrFilesystems = map[string]mbrFilesystem{
	"exfat": {
		partTypes:   []byte{0x07},
		signatureAt: 0x03,
		signature:   "EXFAT   ",
	},
	"fat32": {
		partTypes:   []byte{0x0B, 0x0C},
		signatureAt: 0x52,
		signature:   "FAT32   ",
	},
	"vfat": {
		partTypes:   []byte{0x0B, 0x0C},
		signatureAt: 0x52,
		signature:   "FAT32   ",
	},
}

func findMBRPartition(f *os.File, filesystem string) (Partition, error) {
	fs, ok := mbrFilesystems[filesystem]
	if !ok {
		return Partition{}, fmt.Errorf("unsupported filesystem %q", filesystem)
	}

	var mbr [sectorSize]byte
	if _, err := f.ReadAt(mbr[:], 0); err != nil {
		return Partition{}, fmt.Errorf("read MBR: %w", err)
	}

	if binary.LittleEndian.Uint16(mbr[510:512]) != 0xAA55 {
		return Partition{}, fmt.Errorf("invalid MBR signature")
	}

	for i := 0; i < 4; i++ {
		off := 446 + i*16
		partType := mbr[off+4]

		if !containsByte(fs.partTypes, partType) {
			continue
		}

		startLBA := binary.LittleEndian.Uint32(mbr[off+8 : off+12])
		sectors := binary.LittleEndian.Uint32(mbr[off+12 : off+16])
		if startLBA == 0 || sectors == 0 {
			continue
		}

		partitionOffset := int64(startLBA) * sectorSize

		var boot [sectorSize]byte
		if _, err := f.ReadAt(boot[:], partitionOffset); err != nil {
			continue
		}

		end := fs.signatureAt + len(fs.signature)
		if end > len(boot) || string(boot[fs.signatureAt:end]) != fs.signature {
			continue
		}

		return Partition{
			Offset: partitionOffset,
			Size:   int64(sectors) * sectorSize,
			Type:   partType,
		}, nil
	}

	return Partition{}, fmt.Errorf("no %s partition found in MBR", filesystem)
}
