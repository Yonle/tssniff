package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"unicode/utf16"
)

type ExfatTracker struct {
	fd        *os.File
	partition Partition

	*rangeStore
}

func NewExfatTracker(
	fd *os.File,
	partition Partition,
) *ExfatTracker {
	return &ExfatTracker{
		fd:         fd,
		partition:  partition,
		rangeStore: newRangeStore(),
	}
}

func (t *ExfatTracker) Refresh() error {
	layout, err := readExfatLayout(t.fd, t.partition)
	if err != nil {
		return err
	}

	clusterBase := layout.partitionOffset + layout.clusterHeapOffsetBytes
	fatBase := layout.partitionOffset +
		(layout.fatOffset+uint64(layout.activeFAT)*layout.fatLength)*
			layout.sectorSize

	state := &exfatScanState{
		fd:          t.fd,
		layout:      layout,
		clusterBase: clusterBase,
		visited:     make(map[uint32]bool),
		meta: []ByteRange{{
			Start: uint64(t.partition.Offset),
			End: uint64(t.partition.Offset) +
				layout.clusterHeapOffsetBytes,
			Kind: RangeMeta,
			Name: "boot+FAT",
		}},
		fat: newFATReader(
			t.fd,
			fatBase,
			layout.clusterCount,
			0x0FFFFFFF,
			0xFFFFFFF8,
			0xFFFFFFF7,
			false,
		),
		maxDepth: 32,
	}

	chain, err := state.fat.chain(
		uint64(layout.rootCluster),
		layout.clusterCount,
		"exFAT",
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

type exfatLayout struct {
	partitionOffset uint64

	sectorSize        uint64
	sectorsPerCluster uint64
	clusterSize       uint64

	fatOffset uint64
	fatLength uint64

	clusterHeapOffset      uint64
	clusterHeapOffsetBytes uint64
	clusterCount           uint64

	rootCluster uint32
	activeFAT   uint32

	volumeLength uint64
}

func readExfatLayout(
	fd *os.File,
	partition Partition,
) (exfatLayout, error) {
	if partition.Offset < 0 {
		return exfatLayout{}, fmt.Errorf(
			"negative partition offset %d",
			partition.Offset,
		)
	}

	boot := make([]byte, 512)
	if _, err := fd.ReadAt(boot, partition.Offset); err != nil {
		return exfatLayout{}, err
	}

	if string(boot[3:11]) != "EXFAT   " {
		return exfatLayout{}, fmt.Errorf("not an exFAT volume")
	}

	if binary.LittleEndian.Uint16(boot[510:512]) != 0xAA55 {
		return exfatLayout{}, fmt.Errorf("invalid exFAT boot signature")
	}

	bpsShift := boot[0x6C]
	spcShift := boot[0x6D]

	if bpsShift < 9 || bpsShift > 12 {
		return exfatLayout{}, fmt.Errorf(
			"unsupported sector shift %d",
			bpsShift,
		)
	}

	sectorSize := uint64(1) << bpsShift
	sectorsPerCluster := uint64(1) << spcShift
	clusterSize := sectorSize * sectorsPerCluster

	fatOffset := uint64(binary.LittleEndian.Uint32(boot[0x50:0x54]))
	fatLength := uint64(binary.LittleEndian.Uint32(boot[0x54:0x58]))
	clusterHeapOffset := uint64(binary.LittleEndian.Uint32(boot[0x58:0x5C]))
	clusterCount := uint64(binary.LittleEndian.Uint32(boot[0x5C:0x60]))
	rootCluster := binary.LittleEndian.Uint32(boot[0x60:0x64])
	volumeLength := binary.LittleEndian.Uint64(boot[0x48:0x50])

	volumeFlags := binary.LittleEndian.Uint16(boot[0x6A:0x6C])
	numberOfFATs := boot[0x6E]

	activeFAT := uint32(0)
	if numberOfFATs == 2 && (volumeFlags&1) != 0 {
		activeFAT = 1
	}

	return exfatLayout{
		partitionOffset:        uint64(partition.Offset),
		sectorSize:             sectorSize,
		sectorsPerCluster:      sectorsPerCluster,
		clusterSize:            clusterSize,
		fatOffset:              fatOffset,
		fatLength:              fatLength,
		clusterHeapOffset:      clusterHeapOffset,
		clusterHeapOffsetBytes: clusterHeapOffset * sectorSize,
		clusterCount:           clusterCount,
		rootCluster:            rootCluster,
		activeFAT:              activeFAT,
		volumeLength:           volumeLength,
	}, nil
}

type exfatScanState struct {
	fd     *os.File
	layout exfatLayout

	clusterBase uint64
	fat         fatReader
	visited     map[uint32]bool

	meta   []ByteRange
	ranges []ByteRange

	maxDepth int
}

func (s *exfatScanState) walkDirectory(
	firstCluster uint32,
	length uint64,
	noFatChain bool,
	parent string,
	depth int,
) error {
	if depth > s.maxDepth ||
		firstCluster < 2 ||
		length == 0 {
		return nil
	}

	chain, err := clustersFor(
		firstCluster,
		length,
		s.layout.clusterSize,
		noFatChain,
		func(first uint32, max uint64) ([]uint32, error) {
			return s.fat.chain(
				uint64(first),
				max,
				"exFAT",
			)
		},
	)
	if err != nil {
		return err
	}

	return s.parseDirectoryClusters(chain, parent, depth)
}

func (s *exfatScanState) parseDirectoryClusters(
	chain []uint32,
	parent string,
	depth int,
) error {
	if depth > s.maxDepth {
		return fmt.Errorf("directory recursion too deep")
	}

	const maxDirectoryBytes = 64 << 20

	total := uint64(len(chain)) * s.layout.clusterSize
	if total > maxDirectoryBytes {
		return fmt.Errorf("directory too large for prototype")
	}

	dir := make([]byte, 0, int(total))

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

		dir = append(dir, buf...)
	}

	for i := 0; i+32 <= len(dir); i += 32 {
		primary := dir[i : i+32]
		entryType := primary[0]

		switch entryType {
		case 0x81, 0x82:
			firstCluster := binary.LittleEndian.Uint32(primary[20:24])
			dataLength := binary.LittleEndian.Uint64(primary[24:32])

			if firstCluster != 0 && dataLength != 0 {
				clusters, err := clustersFor(
					firstCluster,
					dataLength,
					s.layout.clusterSize,
					false,
					func(first uint32, max uint64) ([]uint32, error) {
						return s.fat.chain(
							uint64(first),
							max,
							"exFAT",
						)
					},
				)
				if err != nil {
					return err
				}

				name := "allocation bitmap"
				if entryType == 0x82 {
					name = "up-case table"
				}

				for _, r := range clustersToRanges(
					clusters,
					s.clusterBase,
					s.layout.clusterSize,
					0,
				) {
					r.Kind = RangeMeta
					r.Name = name
					s.meta = append(s.meta, r)
				}
			}
			continue

		case 0x00:
			return nil
		case 0x85:
		default:
			continue
		}

		secondaryCount := int(primary[1])
		setEnd := i + 32*(1+secondaryCount)
		if secondaryCount <= 0 || setEnd > len(dir) {
			continue
		}

		var stream []byte
		nameWords := make([]uint16, 0, 255)

		for j := 0; j < secondaryCount; j++ {
			secondary := dir[i+32*(j+1) : i+32*(j+2)]

			switch secondary[0] {
			case 0xC0:
				stream = secondary

			case 0xC1:
				for k := 0; k < 15; k++ {
					nameWords = append(
						nameWords,
						binary.LittleEndian.Uint16(
							secondary[2+k*2:4+k*2],
						),
					)
				}
			}
		}

		if stream == nil {
			continue
		}

		nameLength := int(stream[3])
		if nameLength > len(nameWords) {
			nameLength = len(nameWords)
		}

		name := string(utf16.Decode(nameWords[:nameLength]))
		fullName := name
		if parent != "" {
			fullName = parent + "/" + name
		}

		flags := stream[1]
		noFatChain := flags&0x02 != 0
		firstCluster := binary.LittleEndian.Uint32(stream[20:24])
		dataLength := binary.LittleEndian.Uint64(stream[24:32])

		attributes := binary.LittleEndian.Uint16(primary[4:6])
		isDirectory := attributes&0x10 != 0

		if firstCluster == 0 || dataLength == 0 {
			continue
		}

		clusters, err := clustersFor(
			firstCluster,
			dataLength,
			s.layout.clusterSize,
			noFatChain,
			func(first uint32, max uint64) ([]uint32, error) {
				return s.fat.chain(
					uint64(first),
					max,
					"exFAT",
				)
			},
		)
		if err != nil {
			return fmt.Errorf("%s: %w", fullName, err)
		}

		ranges := clustersToRanges(
			clusters,
			s.clusterBase,
			s.layout.clusterSize,
			dataLength,
		)

		if isDirectory {
			if err := s.walkDirectory(
				firstCluster,
				dataLength,
				noFatChain,
				fullName,
				depth+1,
			); err != nil {
				return err
			}
			continue
		}

		kind := RangeNormal
		if isTSFile(fullName) {
			kind = RangeTS
		}

		for _, r := range ranges {
			r.Kind = kind
			r.Name = fullName
			s.ranges = append(s.ranges, r)
		}

		i = setEnd - 32
	}

	return nil
}
