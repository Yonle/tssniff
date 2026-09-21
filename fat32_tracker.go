package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"sync"
	"unicode/utf16"
)

type FAT32Tracker struct {
	fd        *os.File
	partition Partition

	mu    sync.RWMutex
	meta  []ByteRange
	rules []ByteRange
}

func NewFAT32Tracker(
	fd *os.File,
	partition Partition,
) (*FAT32Tracker, error) {
	if _, err := readFAT32Layout(
		fd,
		partition,
	); err != nil {
		return nil, err
	}

	return &FAT32Tracker{
		fd:        fd,
		partition: partition,
	}, nil
}

func (t *FAT32Tracker) Classify(
	start,
	length uint64,
) []ByteRange {
	if length == 0 {
		return nil
	}

	end := start + length

	t.mu.RLock()
	defer t.mu.RUnlock()

	bounds := []uint64{start, end}

	addBounds := func(r ByteRange) {
		if r.End <= start || r.Start >= end {
			return
		}

		if r.Start > start && r.Start < end {
			bounds = append(bounds, r.Start)
		}

		if r.End > start && r.End < end {
			bounds = append(bounds, r.End)
		}
	}

	for _, r := range t.meta {
		addBounds(r)
	}

	for _, r := range t.rules {
		addBounds(r)
	}

	sort.Slice(bounds, func(i, j int) bool {
		return bounds[i] < bounds[j]
	})

	uniq := bounds[:0]

	for _, b := range bounds {
		if len(uniq) == 0 ||
			uniq[len(uniq)-1] != b {
			uniq = append(uniq, b)
		}
	}

	var out []ByteRange

	for i := 0; i+1 < len(uniq); i++ {
		a := uniq[i]
		b := uniq[i+1]

		if a == b {
			continue
		}

		kind := RangeUnknown
		name := ""

		// Metadata gets priority.
		for _, r := range t.meta {
			if r.Start <= a && b <= r.End {
				kind = RangeMeta
				name = r.Name
				break
			}
		}

		if kind == RangeUnknown {
			for _, r := range t.rules {
				if r.Start <= a && b <= r.End {
					kind = r.Kind
					name = r.Name
					break
				}
			}
		}

		out = append(out, ByteRange{
			Start: a,
			End:   b,
			Kind:  kind,
			Name:  name,
		})
	}

	return coalesceRanges(out)
}

func (t *FAT32Tracker) Snapshot() []ByteRange {
	t.mu.RLock()
	defer t.mu.RUnlock()

	out := make([]ByteRange, len(t.rules))
	copy(out, t.rules)

	return out
}

func (t *FAT32Tracker) OverlapsMetadata(
	start,
	length uint64,
) bool {
	if length == 0 {
		return false
	}

	end := start + length

	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, r := range t.meta {
		if r.Start < end && start < r.End {
			return true
		}
	}

	return false
}

func (t *FAT32Tracker) Refresh() error {
	layout, err := readFAT32Layout(
		t.fd,
		t.partition,
	)
	if err != nil {
		return err
	}

	state := &fat32ScanState{
		fd:     t.fd,
		layout: layout,

		visited: make(map[uint32]bool),

		meta: []ByteRange{
			{
				Start: uint64(t.partition.Offset),
				End: uint64(t.partition.Offset) +
					layout.dataOffsetBytes,
				Kind: RangeMeta,
				Name: "boot+FAT",
			},
		},

		maxDepth: 32,
	}

	if err := state.walkRoot(); err != nil {
		return err
	}

	sort.Slice(
		state.ranges,
		func(i, j int) bool {
			return state.ranges[i].Start <
				state.ranges[j].Start
		},
	)

	sort.Slice(
		state.meta,
		func(i, j int) bool {
			return state.meta[i].Start <
				state.meta[j].Start
		},
	)

	t.mu.Lock()
	t.meta = coalesceRanges(state.meta)
	t.rules = coalesceRanges(state.ranges)
	t.mu.Unlock()

	log.Printf(
		"tracker refresh: %d meta ranges, %d file ranges",
		len(t.meta),
		len(t.rules),
	)

	return nil
}

type fat32Layout struct {
	partitionOffset uint64

	sectorSize        uint64
	sectorsPerCluster uint64
	clusterSize       uint64

	reservedSectors uint64
	numberOfFATs    uint64
	fatSize         uint64

	totalSectors uint64
	dataSectors  uint64
	clusterCount uint64

	rootCluster uint32

	fatOffset         uint64
	dataOffset        uint64
	dataOffsetBytes   uint64
	fatLengthBytes    uint64
	volumeLengthBytes uint64
}

func readFAT32Layout(
	fd *os.File,
	partition Partition,
) (fat32Layout, error) {
	if partition.Offset < 0 {
		return fat32Layout{},
			fmt.Errorf(
				"negative partition offset %d",
				partition.Offset,
			)
	}

	if partition.Size <= 0 {
		return fat32Layout{},
			fmt.Errorf(
				"invalid partition size %d",
				partition.Size,
			)
	}

	var boot [512]byte

	if _, err := fd.ReadAt(
		boot[:],
		partition.Offset,
	); err != nil {
		return fat32Layout{}, fmt.Errorf(
			"read FAT32 boot sector: %w",
			err,
		)
	}

	if binary.LittleEndian.Uint16(
		boot[510:512],
	) != 0xAA55 {
		return fat32Layout{},
			fmt.Errorf(
				"invalid FAT32 boot signature",
			)
	}

	bps := binary.LittleEndian.Uint16(
		boot[0x0B:0x0D],
	)

	switch bps {
	case 512, 1024, 2048, 4096:
	default:
		return fat32Layout{},
			fmt.Errorf(
				"unsupported bytes/sector %d",
				bps,
			)
	}

	sectorsPerCluster := uint64(boot[0x0D])

	if sectorsPerCluster == 0 ||
		(sectorsPerCluster&(sectorsPerCluster-1)) != 0 {
		return fat32Layout{},
			fmt.Errorf(
				"invalid sectors/cluster %d",
				sectorsPerCluster,
			)
	}

	reservedSectors := uint64(
		binary.LittleEndian.Uint16(
			boot[0x0E:0x10],
		),
	)

	numberOfFATs := uint64(boot[0x10])

	if reservedSectors == 0 {
		return fat32Layout{},
			fmt.Errorf(
				"invalid reserved sector count",
			)
	}

	if numberOfFATs == 0 ||
		numberOfFATs > 2 {
		return fat32Layout{},
			fmt.Errorf(
				"invalid FAT count %d",
				numberOfFATs,
			)
	}

	// FAT16 field must be zero on FAT32.
	if binary.LittleEndian.Uint16(
		boot[0x16:0x18],
	) != 0 {
		return fat32Layout{},
			fmt.Errorf(
				"FAT16 FAT size field is non-zero",
			)
	}

	fatSize := uint64(
		binary.LittleEndian.Uint32(
			boot[0x24:0x28],
		),
	)

	if fatSize == 0 {
		return fat32Layout{},
			fmt.Errorf(
				"zero FAT32 size",
			)
	}

	totalSectors := uint64(
		binary.LittleEndian.Uint32(
			boot[0x20:0x24],
		),
	)

	if totalSectors == 0 {
		return fat32Layout{},
			fmt.Errorf(
				"zero FAT32 sector count",
			)
	}

	partitionSectors :=
		uint64(partition.Size) /
			uint64(bps)

	if totalSectors > partitionSectors {
		return fat32Layout{},
			fmt.Errorf(
				"FAT32 volume exceeds partition",
			)
	}

	dataStartSector :=
		reservedSectors +
			numberOfFATs*fatSize

	if dataStartSector >= totalSectors {
		return fat32Layout{},
			fmt.Errorf(
				"FAT32 data region outside volume",
			)
	}

	dataSectors :=
		totalSectors -
			dataStartSector

	clusterCount :=
		dataSectors /
			sectorsPerCluster

	if clusterCount == 0 {
		return fat32Layout{},
			fmt.Errorf(
				"zero FAT32 cluster count",
			)
	}

	rootCluster :=
		binary.LittleEndian.Uint32(
			boot[0x2C:0x30],
		)

	if rootCluster < 2 ||
		uint64(rootCluster) > clusterCount+1 {
		return fat32Layout{},
			fmt.Errorf(
				"invalid root cluster %d",
				rootCluster,
			)
	}

	sectorSize := uint64(bps)

	clusterSize :=
		sectorSize *
			sectorsPerCluster

	fatOffset :=
		uint64(partition.Offset) +
			reservedSectors*sectorSize

	dataOffset :=
		uint64(partition.Offset) +
			dataStartSector*sectorSize

	fatLengthBytes :=
		fatSize *
			sectorSize

	dataOffsetBytes :=
		dataStartSector *
			sectorSize

	volumeLengthBytes :=
		totalSectors *
			sectorSize

	return fat32Layout{
		partitionOffset: uint64(partition.Offset),

		sectorSize:        sectorSize,
		sectorsPerCluster: sectorsPerCluster,
		clusterSize:       clusterSize,

		reservedSectors: reservedSectors,
		numberOfFATs:    numberOfFATs,
		fatSize:         fatSize,

		totalSectors: totalSectors,
		dataSectors:  dataSectors,
		clusterCount: clusterCount,

		rootCluster: rootCluster,

		fatOffset:         fatOffset,
		dataOffset:        dataOffset,
		dataOffsetBytes:   dataOffsetBytes,
		fatLengthBytes:    fatLengthBytes,
		volumeLengthBytes: volumeLengthBytes,
	}, nil
}

type fat32ScanState struct {
	fd     *os.File
	layout fat32Layout

	visited map[uint32]bool

	meta   []ByteRange
	ranges []ByteRange

	maxDepth int

	fatBuf  [512]byte
	fatBase uint64
	fatSet  bool
}

func (s *fat32ScanState) clusterOffset(
	cluster uint32,
) (uint64, error) {
	if cluster < 2 ||
		uint64(cluster) > s.layout.clusterCount+1 {
		return 0,
			fmt.Errorf(
				"invalid cluster %d",
				cluster,
			)
	}

	sector :=
		(uint64(cluster) - 2) *
			s.layout.sectorsPerCluster

	return s.layout.dataOffset +
		sector*s.layout.sectorSize, nil
}

func (s *fat32ScanState) fatOffset(
	cluster uint32,
) uint64 {
	return s.layout.fatOffset +
		uint64(cluster)*4
}

func (s *fat32ScanState) nextCluster(cluster uint32) (uint32, error) {
	off := s.fatOffset(cluster)
	base := off &^ 511

	if !s.fatSet || s.fatBase != base {
		if _, err := s.fd.ReadAt(s.fatBuf[:], int64(base)); err != nil {
			return 0, err
		}
		s.fatBase, s.fatSet = base, true
	}

	i := off - base
	return binary.LittleEndian.Uint32(s.fatBuf[i:i+4]) & 0x0FFFFFFF, nil
}

func isFAT32EOC(c uint32) bool {
	return c >= 0x0FFFFFF8
}

func (s *fat32ScanState) fatChain(
	first uint32,
	maxClusters uint64,
) ([]uint32, error) {
	if first < 2 {
		return nil, nil
	}

	chain := make(
		[]uint32,
		0,
		minU64(maxClusters, 1024),
	)

	seen := make(
		map[uint32]bool,
	)

	cur := first

	for uint64(len(chain)) < maxClusters {
		if cur < 2 ||
			uint64(cur) > s.layout.clusterCount+1 {
			return nil,
				fmt.Errorf(
					"invalid FAT32 cluster %d",
					cur,
				)
		}

		if seen[cur] {
			return nil,
				fmt.Errorf(
					"FAT32 loop at cluster %d",
					cur,
				)
		}

		seen[cur] = true
		chain = append(chain, cur)

		next, err := s.nextCluster(cur)
		if err != nil {
			return nil, err
		}

		if isFAT32EOC(next) {
			break
		}

		if next == 0x0FFFFFF7 {
			return nil,
				fmt.Errorf(
					"bad cluster marker at %d",
					cur,
				)
		}

		if next == 0 {
			return nil,
				fmt.Errorf(
					"free cluster in chain at %d",
					cur,
				)
		}

		cur = next
	}

	return chain, nil
}

func (s *fat32ScanState) walkRoot() error {
	chain, err := s.fatChain(
		s.layout.rootCluster,
		s.layout.clusterCount,
	)
	if err != nil {
		return err
	}

	return s.parseDirectoryClusters(
		chain,
		"",
		0,
	)
}

func (s *fat32ScanState) walkSubdir(
	firstCluster uint32,
	parent string,
	depth int,
) error {
	if depth > s.maxDepth ||
		firstCluster < 2 {
		return nil
	}

	chain, err := s.fatChain(
		firstCluster,
		s.layout.clusterCount,
	)
	if err != nil {
		return err
	}

	return s.parseDirectoryClusters(
		chain,
		parent,
		depth,
	)
}

type fat32LFN struct {
	parts    map[uint8][]uint16
	checksum byte
	maxSeq   uint8
	valid    bool
}

func (l *fat32LFN) reset() {
	l.parts = nil
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
		l.parts = make(map[uint8][]uint16)
		l.maxSeq = seq
		l.checksum = entry[13]
		l.valid = true
	} else if !l.valid ||
		entry[13] != l.checksum ||
		seq >= l.maxSeq {
		l.reset()
		return
	}

	words := make([]uint16, 0, 13)

	addWords := func(a, b int) {
		for i := a; i < b; i += 2 {
			v := binary.LittleEndian.Uint16(
				entry[i : i+2],
			)

			words = append(words, v)
		}
	}

	addWords(1, 11)
	addWords(14, 26)
	addWords(28, 32)

	l.parts[seq] = words
}

func (l *fat32LFN) name(shortName []byte) string {
	if !l.valid ||
		l.maxSeq == 0 ||
		len(l.parts) != int(l.maxSeq) {
		return shortNameString(shortName)
	}

	var words []uint16

	for seq := uint8(1); seq <= l.maxSeq; seq++ {
		part, ok := l.parts[seq]
		if !ok {
			return shortNameString(shortName)
		}

		words = append(words, part...)
	}

	// Remove VFAT filler/terminator.
	n := len(words)

	for i, v := range words {
		if v == 0x0000 || v == 0xFFFF {
			n = i
			break
		}
	}

	return string(
		utf16.Decode(words[:n]),
	)
}

func fat32ShortNameChecksum(name []byte) byte {
	var sum byte

	for i := 0; i < 11; i++ {
		sum = ((sum & 1) << 7) +
			(sum >> 1) +
			name[i]
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

	for i := 1; i < 8; i++ {
		if entry[i] == ' ' {
			break
		}

		name = append(name, entry[i])
	}

	extStart := len(name)

	for i := 8; i < 11; i++ {
		if entry[i] == ' ' {
			break
		}

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
		return fmt.Errorf(
			"directory recursion too deep",
		)
	}

	const maxDirectoryBytes = 64 << 20

	total := uint64(len(chain)) *
		s.layout.clusterSize

	if total > maxDirectoryBytes {
		return fmt.Errorf(
			"directory too large for prototype",
		)
	}

	var lfn fat32LFN

	for _, cluster := range chain {
		if s.visited[cluster] {
			continue
		}

		s.visited[cluster] = true

		off, err := s.clusterOffset(cluster)
		if err != nil {
			return err
		}

		s.meta = append(
			s.meta,
			ByteRange{
				Start: off,
				End:   off + s.layout.clusterSize,
				Kind:  RangeMeta,
				Name:  "directory",
			},
		)

		buf := make(
			[]byte,
			int(s.layout.clusterSize),
		)

		if _, err := s.fd.ReadAt(
			buf,
			int64(off),
		); err != nil && err != io.EOF {
			return err
		}

		for i := 0; i+32 <= len(buf); i += 32 {
			entry := buf[i : i+32]
			entryType := entry[0]

			switch entryType {
			case 0x00:
				return nil

			case 0xE5:
				// Deleted entry.
				lfn.reset()
				continue

			case 0x0F:
				// VFAT long filename entry.
				lfn.add(entry)
				continue
			}

			attributes := binary.LittleEndian.Uint16(
				entry[11:13],
			)

			// Volume labels aren't files.
			if attributes&0x08 != 0 {
				lfn.reset()
				continue
			}

			// Only ordinary files/directories here.
			if attributes&0xC0 != 0 {
				lfn.reset()
				continue
			}

			shortName := entry[:11]
			name := lfn.name(shortName)

			// Validate LFN checksum. If it doesn't match,
			// fall back to the short name.
			if lfn.valid &&
				fat32ShortNameChecksum(shortName) !=
					lfn.checksum {
				name = shortNameString(shortName)
			}

			lfn.reset()

			if name == "" ||
				name == "." ||
				name == ".." {
				continue
			}

			fullName := name

			if parent != "" {
				fullName =
					parent +
						"/" +
						name
			}

			firstClusterHigh :=
				binary.LittleEndian.Uint16(
					entry[20:22],
				)

			firstClusterLow :=
				binary.LittleEndian.Uint16(
					entry[26:28],
				)

			firstCluster :=
				(uint32(firstClusterHigh) << 16) |
					uint32(firstClusterLow)

			isDirectory :=
				attributes&0x10 != 0

			if firstCluster < 2 {
				continue
			}

			if isDirectory {
				if err := s.walkSubdir(
					firstCluster,
					fullName,
					depth+1,
				); err != nil {
					return err
				}

				continue
			}

			dataLength :=
				uint64(binary.LittleEndian.Uint32(
					entry[28:32],
				))

			if dataLength == 0 {
				continue
			}

			/*
					IMPORTANT: we deliberately ignore dataLength here.

					A DTV STB pre-allocates a long cluster chain in the FAT
					and streams into it, updating the directory entry's size
					only occasionally (or at stop time). If we truncate the
					chain at dataLength, every cluster the STB is currently
					writing into looks "free" to us and gets quarantined
					forever.

				  	We therefore walk the whole chain and treat every cluster
				   	as belonging to the file.
			*/
			clusters, err := s.fatChain(
				firstCluster,
				(dataLength+s.layout.clusterSize-1)/s.layout.clusterSize,
			)
			if err != nil {
				return fmt.Errorf(
					"%s: %w",
					fullName,
					err,
				)
			}

			ranges := clustersToFAT32FileRanges(
				clusters,
				s.layout.clusterSize,
				dataLength,
				s,
			)

			kind := RangeNormal

			if isTSFile(fullName) {
				kind = RangeTS
			}

			for _, r := range ranges {
				r.Kind = kind
				r.Name = fullName

				s.ranges = append(
					s.ranges,
					r,
				)
			}
		}
	}

	return nil
}

func clustersToFAT32Ranges(
	clusters []uint32,
	clusterSize uint64,
	s *fat32ScanState,
) []ByteRange {
	if len(clusters) == 0 {
		return nil
	}

	ranges := make(
		[]ByteRange,
		0,
		4,
	)

	start := clusters[0]
	prev := start

	flush := func(a, b uint32) {
		startOff, err := s.clusterOffset(a)
		if err != nil {
			return
		}

		endOff, err := s.clusterOffset(b)
		if err != nil {
			return
		}

		endOff += clusterSize

		ranges = append(
			ranges,
			ByteRange{
				Start: startOff,
				End:   endOff,
			},
		)
	}

	for _, cluster := range clusters[1:] {
		if cluster == prev+1 {
			prev = cluster
			continue
		}

		flush(start, prev)

		start = cluster
		prev = cluster
	}

	flush(start, prev)

	return ranges
}

func clustersToFAT32FileRanges(
	clusters []uint32,
	clusterSize uint64,
	fileLength uint64,
	s *fat32ScanState,
) []ByteRange {
	if len(clusters) == 0 || fileLength == 0 {
		return nil
	}

	ranges := make([]ByteRange, 0, 4)

	start := clusters[0]
	prev := start
	remaining := fileLength

	flush := func(a, b uint32) {
		startOff, _ := s.clusterOffset(a)
		endOff, _ := s.clusterOffset(b)

		clusterCount := uint64(b-a) + 1
		runSize := clusterCount * clusterSize

		if runSize > remaining {
			runSize = remaining
		}

		endOff = startOff + runSize

		ranges = append(ranges, ByteRange{
			Start: startOff,
			End:   endOff,
		})

		remaining -= runSize
	}

	for _, cluster := range clusters[1:] {
		if cluster == prev+1 {
			prev = cluster
			continue
		}

		flush(start, prev)

		if remaining == 0 {
			break
		}

		start = cluster
		prev = cluster
	}

	if remaining > 0 {
		flush(start, prev)
	}

	return ranges
}

func clustersToFAT32FullRanges(
	clusters []uint32,
	clusterSize uint64,
	s *fat32ScanState,
) []ByteRange {
	if len(clusters) == 0 {
		return nil
	}

	ranges := make([]ByteRange, 0, 4)

	start := clusters[0]
	prev := start

	flush := func(a, b uint32) {
		startOff, err := s.clusterOffset(a)
		if err != nil {
			return
		}

		endOff, err := s.clusterOffset(b)
		if err != nil {
			return
		}

		endOff += clusterSize

		ranges = append(ranges, ByteRange{
			Start: startOff,
			End:   endOff,
		})
	}

	for _, cluster := range clusters[1:] {
		if cluster == prev+1 {
			prev = cluster
			continue
		}

		flush(start, prev)

		start = cluster
		prev = cluster
	}

	flush(start, prev)

	return ranges
}

func (s *fat32ScanState) walkSubdirFull(
	firstCluster uint32,
	parent string,
	depth int,
) error {
	if depth > s.maxDepth || firstCluster < 2 {
		return nil
	}

	chain, err := s.fatChain(
		firstCluster,
		s.layout.clusterCount,
	)
	if err != nil {
		return err
	}

	return s.parseDirectoryClusters(
		chain,
		parent,
		depth,
	)
}
