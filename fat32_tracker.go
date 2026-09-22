package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"unicode/utf16"
)

type FAT32Tracker struct {
	fd        *os.File
	partition Partition

	*rangeStore
}

func NewFAT32Tracker(
	fd *os.File,
	partition Partition,
) (*FAT32Tracker, error) {
	if _, err := readFAT32Layout(fd, partition); err != nil {
		return nil, err
	}

	return &FAT32Tracker{
		fd:         fd,
		partition:  partition,
		rangeStore: newRangeStore(),
	}, nil
}

func (t *FAT32Tracker) Refresh() error {
	layout, err := readFAT32Layout(t.fd, t.partition)
	if err != nil {
		return err
	}

	state := &fat32ScanState{
		fd:          t.fd,
		layout:      layout,
		clusterBase: layout.dataOffset,
		visited:     make(map[uint32]bool),
		meta: []ByteRange{{
			Start: uint64(t.partition.Offset),
			End: uint64(t.partition.Offset) +
				layout.dataOffsetBytes,
			Kind: RangeMeta,
			Name: "boot+FAT",
		}},
		fat: newFATReader(
			t.fd,
			layout.fatOffset,
			layout.clusterCount,
			0x0FFFFFFF,
			0x0FFFFFF8,
			0x0FFFFFF7,
			true,
		),
		maxDepth: 32,
	}

	chain, err := state.fat.chain(
		uint64(layout.rootCluster),
		layout.clusterCount,
		"FAT32",
	)
	if err != nil {
		return err
	}

	if err := state.parseDirectoryClusters(chain, "", 0); err != nil {
		return err
	}

	sortRanges(state.ranges)
	sortRanges(state.meta)

	t.set(
		coalesceRanges(state.meta),
		coalesceRanges(state.ranges),
	)

	log.Printf(
		"tracker refresh: %d meta ranges, %d file ranges",
		len(state.meta),
		len(state.ranges),
	)

	return nil
}

type fat32Layout struct {
	sectorSize        uint64
	sectorsPerCluster uint64
	clusterSize       uint64

	clusterCount uint64
	rootCluster  uint32

	fatOffset       uint64
	dataOffset      uint64
	dataOffsetBytes uint64
}

func readFAT32Layout(
	fd *os.File,
	partition Partition,
) (fat32Layout, error) {
	if partition.Offset < 0 {
		return fat32Layout{}, fmt.Errorf(
			"negative partition offset %d",
			partition.Offset,
		)
	}
	if partition.Size <= 0 {
		return fat32Layout{}, fmt.Errorf(
			"invalid partition size %d",
			partition.Size,
		)
	}

	var boot [512]byte
	if _, err := fd.ReadAt(boot[:], partition.Offset); err != nil {
		return fat32Layout{}, fmt.Errorf(
			"read FAT32 boot sector: %w",
			err,
		)
	}

	if binary.LittleEndian.Uint16(boot[510:512]) != 0xAA55 {
		return fat32Layout{}, fmt.Errorf("invalid FAT32 boot signature")
	}

	bps := binary.LittleEndian.Uint16(boot[0x0B:0x0D])
	switch bps {
	case 512, 1024, 2048, 4096:
	default:
		return fat32Layout{}, fmt.Errorf(
			"unsupported bytes/sector %d",
			bps,
		)
	}

	sectorsPerCluster := uint64(boot[0x0D])
	if sectorsPerCluster == 0 ||
		sectorsPerCluster&(sectorsPerCluster-1) != 0 {
		return fat32Layout{}, fmt.Errorf(
			"invalid sectors/cluster %d",
			sectorsPerCluster,
		)
	}

	reservedSectors := uint64(
		binary.LittleEndian.Uint16(boot[0x0E:0x10]),
	)
	numberOfFATs := uint64(boot[0x10])

	if reservedSectors == 0 {
		return fat32Layout{}, fmt.Errorf(
			"invalid reserved sector count",
		)
	}
	if numberOfFATs == 0 || numberOfFATs > 2 {
		return fat32Layout{}, fmt.Errorf(
			"invalid FAT count %d",
			numberOfFATs,
		)
	}

	if binary.LittleEndian.Uint16(boot[0x16:0x18]) != 0 {
		return fat32Layout{}, fmt.Errorf(
			"FAT16 FAT size field is non-zero",
		)
	}

	fatSize := uint64(binary.LittleEndian.Uint32(boot[0x24:0x28]))
	if fatSize == 0 {
		return fat32Layout{}, fmt.Errorf("zero FAT32 size")
	}

	totalSectors := uint64(binary.LittleEndian.Uint32(boot[0x20:0x24]))
	if totalSectors == 0 {
		return fat32Layout{}, fmt.Errorf("zero FAT32 sector count")
	}

	partitionSectors := uint64(partition.Size) / uint64(bps)
	if totalSectors > partitionSectors {
		return fat32Layout{}, fmt.Errorf(
			"FAT32 volume exceeds partition",
		)
	}

	dataStartSector := reservedSectors + numberOfFATs*fatSize
	if dataStartSector >= totalSectors {
		return fat32Layout{}, fmt.Errorf(
			"FAT32 data region outside volume",
		)
	}

	dataSectors := totalSectors - dataStartSector
	clusterCount := dataSectors / sectorsPerCluster
	if clusterCount == 0 {
		return fat32Layout{}, fmt.Errorf(
			"zero FAT32 cluster count",
		)
	}

	rootCluster := binary.LittleEndian.Uint32(boot[0x2C:0x30])
	if rootCluster < 2 ||
		uint64(rootCluster) > clusterCount+1 {
		return fat32Layout{}, fmt.Errorf(
			"invalid root cluster %d",
			rootCluster,
		)
	}

	sectorSize := uint64(bps)
	clusterSize := sectorSize * sectorsPerCluster
	fatOffset := uint64(partition.Offset) +
		reservedSectors*sectorSize
	dataOffset := uint64(partition.Offset) +
		dataStartSector*sectorSize

	return fat32Layout{
		sectorSize:        sectorSize,
		sectorsPerCluster: sectorsPerCluster,
		clusterSize:       clusterSize,
		clusterCount:      clusterCount,
		rootCluster:       rootCluster,
		fatOffset:         fatOffset,
		dataOffset:        dataOffset,
		dataOffsetBytes:   dataStartSector * sectorSize,
	}, nil
}

type fat32ScanState struct {
	fd     *os.File
	layout fat32Layout

	clusterBase uint64
	fat         fatReader
	visited     map[uint32]bool

	meta   []ByteRange
	ranges []ByteRange

	maxDepth int
}

func (s *fat32ScanState) walkDirectory(
	firstCluster uint32,
	parent string,
	depth int,
) error {
	if depth > s.maxDepth || firstCluster < 2 {
		return nil
	}

	chain, err := s.fat.chain(
		uint64(firstCluster),
		s.layout.clusterCount,
		"FAT32",
	)
	if err != nil {
		return err
	}

	return s.parseDirectoryClusters(chain, parent, depth)
}

type fat32LFN struct {
	words    [20 * 13]uint16
	mask     uint32
	checksum byte
	maxSeq   uint8
	valid    bool
}

func (l *fat32LFN) reset() {
	l.mask = 0
	l.checksum = 0
	l.maxSeq = 0
	l.valid = false
}

func (l *fat32LFN) add(entry []byte) {
	seq := entry[0] & 0x1F
	if seq == 0 || seq > 20 {
		l.reset()
		return
	}

	if entry[0]&0x40 != 0 {
		l.reset()
		l.maxSeq = seq
		l.checksum = entry[13]
		l.valid = true
	} else if !l.valid ||
		entry[13] != l.checksum ||
		seq >= l.maxSeq {
		l.reset()
		return
	}

	bit := uint32(1) << uint(seq-1)
	if l.mask&bit != 0 {
		l.reset()
		return
	}

	dst := int(seq-1) * 13
	for _, span := range [][2]int{
		{1, 11},
		{14, 26},
		{28, 32},
	} {
		for i := span[0]; i < span[1]; i += 2 {
			l.words[dst] = binary.LittleEndian.Uint16(entry[i : i+2])
			dst++
		}
	}

	l.mask |= bit
}

func (l *fat32LFN) name(shortName []byte) string {
	if !l.valid ||
		l.maxSeq == 0 ||
		l.mask != (uint32(1)<<l.maxSeq)-1 {
		return shortNameString(shortName)
	}

	words := l.words[:int(l.maxSeq)*13]
	n := len(words)
	for i, v := range words {
		if v == 0x0000 || v == 0xFFFF {
			n = i
			break
		}
	}

	return string(utf16.Decode(words[:n]))
}

func fat32ShortNameChecksum(name []byte) byte {
	var sum byte
	for i := 0; i < 11; i++ {
		sum = ((sum & 1) << 7) + (sum >> 1) + name[i]
	}
	return sum
}

func shortNameString(entry []byte) string {
	name := make([]byte, 0, 13)

	first := entry[0]
	if first == 0x05 {
		first = 0xE5
	}
	if first != ' ' {
		name = append(name, first)
	}

	for i := 1; i < 8 && entry[i] != ' '; i++ {
		name = append(name, entry[i])
	}

	extStart := len(name)
	for i := 8; i < 11 && entry[i] != ' '; i++ {
		if i == 8 && extStart > 0 {
			name = append(name, '.')
		}
		name = append(name, entry[i])
	}

	return string(name)
}

func (s *fat32ScanState) parseDirectoryClusters(
	chain []uint32,
	parent string,
	depth int,
) error {
	if depth > s.maxDepth {
		return fmt.Errorf("directory recursion too deep")
	}

	const maxDirectoryBytes = 64 << 20

	if uint64(len(chain))*s.layout.clusterSize > maxDirectoryBytes {
		return fmt.Errorf("directory too large for prototype")
	}

	var lfn fat32LFN

	for _, cluster := range chain {
		if s.visited[cluster] {
			continue
		}
		s.visited[cluster] = true

		off, err := clusterOffset(
			s.clusterBase,
			s.layout.clusterSize,
			s.layout.clusterCount,
			cluster,
		)
		if err != nil {
			return err
		}

		s.meta = append(s.meta, ByteRange{
			Start: off,
			End:   off + s.layout.clusterSize,
			Kind:  RangeMeta,
			Name:  "directory",
		})

		buf := make([]byte, int(s.layout.clusterSize))
		if _, err := s.fd.ReadAt(buf, int64(off)); err != nil && err != io.EOF {
			return err
		}

		for i := 0; i+32 <= len(buf); i += 32 {
			entry := buf[i : i+32]

			switch entry[0] {
			case 0x00:
				return nil

			case 0xE5:
				lfn.reset()
				continue

			case 0x0F:
				lfn.add(entry)
				continue
			}

			attributes := binary.LittleEndian.Uint16(entry[11:13])

			if attributes&0x08 != 0 || attributes&0xC0 != 0 {
				lfn.reset()
				continue
			}

			shortName := entry[:11]
			name := lfn.name(shortName)

			if lfn.valid &&
				fat32ShortNameChecksum(shortName) != lfn.checksum {
				name = shortNameString(shortName)
			}
			lfn.reset()

			if name == "" || name == "." || name == ".." {
				continue
			}

			fullName := name
			if parent != "" {
				fullName = parent + "/" + name
			}

			firstCluster :=
				uint32(binary.LittleEndian.Uint16(entry[20:22]))<<16 |
					uint32(binary.LittleEndian.Uint16(entry[26:28]))
			isDirectory := attributes&0x10 != 0

			if firstCluster < 2 {
				continue
			}

			if isDirectory {
				if err := s.walkDirectory(
					firstCluster,
					fullName,
					depth+1,
				); err != nil {
					log.Printf("FAT32 scan: skipping dir %s: %v", fullName, err)
				}
				continue
			}

			dataLength := uint64(
				binary.LittleEndian.Uint32(entry[28:32]),
			)
			if dataLength == 0 {
				continue
			}

			clusters, err := s.fat.chain(
				uint64(firstCluster),
				(dataLength+s.layout.clusterSize-1)/
					s.layout.clusterSize,
				"FAT32",
			)
			if err != nil {
				log.Printf("FAT32 scan: skipping %s: %v", fullName, err)
				continue
			}

			kind := RangeNormal
			if isTSFile(fullName) {
				kind = RangeTS
			}

			for _, r := range clustersToRanges(
				clusters,
				s.clusterBase,
				s.layout.clusterSize,
				dataLength,
			) {
				r.Kind = kind
				r.Name = fullName
				s.ranges = append(s.ranges, r)
			}
		}
	}

	return nil
}
