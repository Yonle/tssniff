package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
)

type RangeKind uint8

const (
	RangeMeta RangeKind = iota
	RangeNormal
	RangeTS
	RangeUnknown
)

type ByteRange struct {
	Start uint64
	End   uint64 // exclusive

	Kind RangeKind
	Name string
}

type ExfatTracker struct {
	fd              *os.File
	partitionOffset int64

	mu    sync.RWMutex
	meta  []ByteRange
	rules []ByteRange
}

func NewExfatTracker(
	fd *os.File,
	partitionOffset int64,
) *ExfatTracker {
	return &ExfatTracker{
		fd:              fd,
		partitionOffset: partitionOffset,
	}
}

func (t *ExfatTracker) Classify(start, length uint64) []ByteRange {
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

	// Remove duplicate boundaries.
	uniq := bounds[:0]
	for _, b := range bounds {
		if len(uniq) == 0 || uniq[len(uniq)-1] != b {
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

func (t *ExfatTracker) Snapshot() []ByteRange {
	t.mu.RLock()
	defer t.mu.RUnlock()

	out := make([]ByteRange, len(t.rules))
	copy(out, t.rules)

	return out
}

func (t *ExfatTracker) OverlapsMetadata(
	start,
	length uint64,
) bool {
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

func (t *ExfatTracker) Refresh() error {
	layout, err := readLayout(t.fd, t.partitionOffset)
	if err != nil {
		return err
	}

	state := &scanState{
		fd:      t.fd,
		layout:  layout,
		visited: make(map[uint32]bool),

		/*
			Everything before the cluster heap contains filesystem
			structures such as the FAT, so any write there can alter
			our filename → cluster mapping.
		*/
		meta: []ByteRange{
			{
				Start: uint64(t.partitionOffset),
				End:   uint64(t.partitionOffset) + layout.clusterHeapOffsetBytes,
				Kind:  RangeMeta,
				Name:  "boot+FAT",
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
	/*
	fmt.Printf(
		"exFAT: %d TS ranges\n",
		len(state.ranges),
	)

	for _, r := range state.ranges {
		fmt.Printf(
			"  TS %-40s [0x%x, 0x%x)  %d bytes\n",
			r.Name,
			r.Start,
			r.End,
			r.End-r.Start,
		)
	}*/

	return nil
}

type exfatLayout struct {
	partitionOffset uint64
	sectorSize      uint64

	sectorsPerCluster uint64
	clusterSize       uint64

	fatOffset uint64
	fatLength uint64

	clusterHeapOffset      uint64
	clusterHeapOffsetBytes uint64
	clusterCount           uint64

	rootCluster uint32

	activeFAT uint32

	volumeLength uint64
}

func readLayout(
	fd *os.File,
	partitionOffset int64,
) (exfatLayout, error) {
	if partitionOffset < 0 {
		return exfatLayout{},
			fmt.Errorf(
				"negative partition offset %d",
				partitionOffset,
			)
	}

	boot := make([]byte, 512)

	if _, err := fd.ReadAt(
		boot,
		partitionOffset,
	); err != nil {
		return exfatLayout{}, err
	}

	if string(boot[3:11]) != "EXFAT   " {
		return exfatLayout{},
			fmt.Errorf("not an exFAT volume")
	}

	if binary.LittleEndian.Uint16(
		boot[510:512],
	) != 0xAA55 {
		return exfatLayout{},
			fmt.Errorf("invalid exFAT boot signature")
	}

	bpsShift := boot[0x6C]
	spcShift := boot[0x6D]

	if bpsShift < 9 || bpsShift > 12 {
		return exfatLayout{},
			fmt.Errorf(
				"unsupported sector shift %d",
				bpsShift,
			)
	}

	sectorSize := uint64(1) << bpsShift

	sectorsPerCluster :=
		uint64(1) << spcShift

	clusterSize :=
		sectorSize * sectorsPerCluster

	fatOffset :=
		uint64(binary.LittleEndian.Uint32(
			boot[0x50:0x54],
		))

	fatLength :=
		uint64(binary.LittleEndian.Uint32(
			boot[0x54:0x58],
		))

	clusterHeapOffset :=
		uint64(binary.LittleEndian.Uint32(
			boot[0x58:0x5C],
		))

	clusterCount :=
		uint64(binary.LittleEndian.Uint32(
			boot[0x5C:0x60],
		))

	rootCluster :=
		binary.LittleEndian.Uint32(
			boot[0x60:0x64],
		)

	volumeLength :=
		binary.LittleEndian.Uint64(
			boot[0x48:0x50],
		)

	volumeFlags :=
		binary.LittleEndian.Uint16(
			boot[0x6A:0x6C],
		)

	numberOfFATs := boot[0x6E]

	activeFAT := uint32(0)

	if numberOfFATs == 2 &&
		(volumeFlags&1) != 0 {
		activeFAT = 1
	}

	return exfatLayout{
		partitionOffset: uint64(partitionOffset),
		sectorSize:      sectorSize,

		sectorsPerCluster: sectorsPerCluster,
		clusterSize:       clusterSize,

		fatOffset: fatOffset,
		fatLength: fatLength,

		clusterHeapOffset:      clusterHeapOffset,
		clusterHeapOffsetBytes: clusterHeapOffset * sectorSize,
		clusterCount:           clusterCount,

		rootCluster: rootCluster,

		activeFAT: activeFAT,

		volumeLength: volumeLength,
	}, nil
}

type scanState struct {
	fd     *os.File
	layout exfatLayout

	visited map[uint32]bool

	meta   []ByteRange
	ranges []ByteRange

	maxDepth int
}

func (s *scanState) clusterOffset(
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
		s.layout.clusterHeapOffset +
			(uint64(cluster)-2)*
				s.layout.sectorsPerCluster

	return s.layout.partitionOffset + sector*s.layout.sectorSize, nil
}

func (s *scanState) fatOffset(
	cluster uint32,
) uint64 {
	baseSector :=
		s.layout.fatOffset +
			uint64(s.layout.activeFAT)*
				s.layout.fatLength

	return s.layout.partitionOffset +
		baseSector*s.layout.sectorSize +
		uint64(cluster)*4
}

func (s *scanState) nextCluster(
	cluster uint32,
) (uint32, error) {
	var b [4]byte

	if _, err := s.fd.ReadAt(
		b[:],
		int64(s.fatOffset(cluster)),
	); err != nil {
		return 0, err
	}

	return binary.LittleEndian.Uint32(b[:]), nil
}

func isEOC(c uint32) bool {
	return c >= 0xFFFFFFF8
}

func (s *scanState) fatChain(
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
					"invalid FAT cluster %d",
					cur,
				)
		}

		if seen[cur] {
			return nil,
				fmt.Errorf(
					"FAT loop at cluster %d",
					cur,
				)
		}

		seen[cur] = true
		chain = append(chain, cur)

		next, err := s.nextCluster(cur)
		if err != nil {
			return nil, err
		}

		if isEOC(next) {
			break
		}

		if next == 0xFFFFFFF7 {
			return nil,
				fmt.Errorf(
					"bad cluster marker at %d",
					cur,
				)
		}

		cur = next
	}

	return chain, nil
}

func clustersFor(
	first uint32,
	length uint64,
	clusterSize uint64,
	noFatChain bool,
	fatChain func(uint32, uint64) ([]uint32, error),
) ([]uint32, error) {
	if first == 0 || length == 0 {
		return nil, nil
	}

	count :=
		(length + clusterSize - 1) /
			clusterSize

	if noFatChain {
		out := make(
			[]uint32,
			0,
			count,
		)

		for i := uint64(0); i < count; i++ {
			v := uint64(first) + i

			if v > uint64(^uint32(0)) {
				return nil,
					fmt.Errorf(
						"cluster overflow",
					)
			}

			out = append(
				out,
				uint32(v),
			)
		}

		return out, nil
	}

	return fatChain(first, count)
}

func (s *scanState) walkRoot() error {
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

func (s *scanState) walkSubdir(
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
		s.fatChain,
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

func (s *scanState) parseDirectoryClusters(
	chain []uint32,
	parent string,
	depth int,
) error {
	if depth > s.maxDepth {
		return fmt.Errorf(
			"directory recursion too deep",
		)
	}

	/*
		The exFAT specification permits directories up to 256 MiB.

		64 MiB is intentionally chosen as a prototype safety limit.
	*/
	const maxDirectoryBytes = 64 << 20

	total := uint64(len(chain)) *
		s.layout.clusterSize

	if total > maxDirectoryBytes {
		return fmt.Errorf(
			"directory too large for prototype",
		)
	}

	dir := make(
		[]byte,
		0,
		int(total),
	)

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

		dir = append(
			dir,
			buf...,
		)
	}

	return s.parseDirectoryBytes(
		dir,
		parent,
		depth,
	)
}

func (s *scanState) parseDirectoryBytes(
	buf []byte,
	parent string,
	depth int,
) error {
	for i := 0; i+32 <= len(buf); i += 32 {
		primary := buf[i : i+32]

		entryType := primary[0]

		switch entryType {
		case 0x81, 0x82:
			// Allocation Bitmap / Up-case Table.
			// These are filesystem metadata stored in the Cluster Heap.
			firstCluster := binary.LittleEndian.Uint32(
				primary[20:24],
			)

			dataLength := binary.LittleEndian.Uint64(
				primary[24:32],
			)

			if firstCluster != 0 && dataLength != 0 {
				clusters, err := clustersFor(
					firstCluster,
					dataLength,
					s.layout.clusterSize,
					false,
					s.fatChain,
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
					s.layout.clusterSize,
					s,
				) {
					r.Kind = RangeMeta
					r.Name = name
					s.meta = append(s.meta, r)
				}
			}

			continue

		case 0x85:
			// Normal file/directory entry.

		default:
			continue
		}

		if entryType == 0x00 {
			break
		}

		if entryType != 0x85 {
			continue
		}

		secondaryCount := int(primary[1])

		setEnd :=
			i + 32*(1+secondaryCount)

		if secondaryCount <= 0 ||
			setEnd > len(buf) {
			continue
		}

		var stream []byte

		nameWords :=
			make([]uint16, 0, 255)

		for j := 0; j < secondaryCount; j++ {
			secondary := buf[i+32*(j+1) : i+32*(j+2)]

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

		name := string(
			utf16.Decode(
				nameWords[:nameLength],
			),
		)

		fullName := name

		if parent != "" {
			fullName = parent + "/" + name
		}

		flags := stream[1]

		noFatChain :=
			flags&0x02 != 0

		firstCluster :=
			binary.LittleEndian.Uint32(
				stream[20:24],
			)

		dataLength :=
			binary.LittleEndian.Uint64(
				stream[24:32],
			)

		attributes :=
			binary.LittleEndian.Uint16(
				primary[4:6],
			)

		isDirectory :=
			attributes&0x10 != 0

		if firstCluster != 0 &&
			dataLength != 0 {

			clusters, err := clustersFor(
				firstCluster,
				dataLength,
				s.layout.clusterSize,
				noFatChain,
				s.fatChain,
			)
			if err != nil {
				return fmt.Errorf(
					"%s: %w",
					fullName,
					err,
				)
			}

			ranges :=
				clustersToRanges(
					clusters,
					s.layout.clusterSize,
					s,
				)

			if isDirectory {
				if err := s.walkSubdir(
					firstCluster,
					dataLength,
					noFatChain,
					fullName,
					depth+1,
				); err != nil {
					return err
				}
			} else {
				kind := RangeNormal

				if strings.HasSuffix(
					strings.ToLower(fullName),
					".ts",
				) {
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

		i = setEnd - 32
	}

	return nil
}

func clustersToRanges(
	clusters []uint32,
	clusterSize uint64,
	s *scanState,
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
		startOff, _ :=
			s.clusterOffset(a)

		endOff, _ :=
			s.clusterOffset(b)

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

func coalesceRanges(
	in []ByteRange,
) []ByteRange {
	if len(in) == 0 {
		return nil
	}

	out := make(
		[]ByteRange,
		0,
		len(in),
	)

	for _, r := range in {
		if r.End <= r.Start {
			continue
		}

		if len(out) > 0 &&
			out[len(out)-1].End >= r.Start &&
			out[len(out)-1].Kind == r.Kind &&
			out[len(out)-1].Name == r.Name {

			if r.End > out[len(out)-1].End {
				out[len(out)-1].End = r.End
			}

			continue
		}

		out = append(out, r)
	}

	return out
}

func findExFATPartition(f *os.File) (Partition, error) {
	return findMBRExFATPartition(f)
}
