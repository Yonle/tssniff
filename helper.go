package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"syscall"
)

const (
	packetSize   = 188
	minPackets   = 3
	fatCacheSize = 4 << 10
)

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func minU64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func containsByte(values []byte, needle byte) bool {
	for _, v := range values {
		if v == needle {
			return true
		}
	}
	return false
}

func isTSFile(name string) bool {
	name = strings.ToLower(name)
	return strings.HasSuffix(name, ".ts") ||
		strings.HasSuffix(name, ".tsv") ||
		strings.HasSuffix(name, ".m2ts") ||
		strings.HasSuffix(name, ".trp")
}

func findMPEGTSOffset(data []byte) (int, bool) {
	if len(data) < packetSize*minPackets {
		return 0, false
	}

	for start := 0; start < packetSize; start++ {
		if data[start] != 0x47 {
			continue
		}

		packets := 0

		for off := start; off+packetSize <= len(data); off += packetSize {
			if data[off] != 0x47 {
				break
			}

			/*
				Adaptation-field control 00 is reserved,
				so this is a cheap sanity check.
			*/
			if data[off+3]&0x30 == 0 {
				break
			}

			packets++
		}

		if packets >= minPackets {
			return start, true
		}
	}

	return 0, false
}

func toErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno
	}

	return syscall.EIO
}

func coalesceRanges(in []ByteRange) []ByteRange {
	if len(in) == 0 {
		return nil
	}

	out := make([]ByteRange, 0, len(in))
	for _, r := range in {
		if r.End <= r.Start {
			continue
		}

		if len(out) > 0 {
			last := &out[len(out)-1]
			if last.End >= r.Start &&
				last.Kind == r.Kind &&
				last.Name == r.Name {
				if r.End > last.End {
					last.End = r.End
				}
				continue
			}
		}

		out = append(out, r)
	}

	return out
}

func buildClassification(meta, rules []ByteRange) []ByteRange {
	if len(meta) == 0 && len(rules) == 0 {
		return nil
	}

	bounds := make([]uint64, 0, 2*(len(meta)+len(rules)))
	for _, r := range meta {
		if r.End > r.Start {
			bounds = append(bounds, r.Start, r.End)
		}
	}
	for _, r := range rules {
		if r.End > r.Start {
			bounds = append(bounds, r.Start, r.End)
		}
	}

	sort.Slice(bounds, func(i, j int) bool {
		return bounds[i] < bounds[j]
	})

	uniq := bounds[:0]
	for _, b := range bounds {
		if len(uniq) == 0 || uniq[len(uniq)-1] != b {
			uniq = append(uniq, b)
		}
	}

	out := make([]ByteRange, 0, len(uniq)-1)
	for i := 0; i+1 < len(uniq); i++ {
		a, b := uniq[i], uniq[i+1]
		if a == b {
			continue
		}

		kind := RangeUnknown
		name := ""

		for _, r := range meta {
			if r.Start <= a && b <= r.End {
				kind = RangeMeta
				name = r.Name
				break
			}
		}
		if kind == RangeUnknown {
			for _, r := range rules {
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

	count := (length + clusterSize - 1) / clusterSize

	if !noFatChain {
		return fatChain(first, count)
	}

	if count > uint64(^uint32(0))-uint64(first)+1 {
		return nil, fmt.Errorf("cluster overflow")
	}

	out := make([]uint32, count)
	for i := uint64(0); i < count; i++ {
		out[i] = first + uint32(i)
	}
	return out, nil
}

func clusterOffset(base, clusterSize, clusterCount uint64, cluster uint32) (uint64, error) {
	if cluster < 2 || uint64(cluster) > clusterCount+1 {
		return 0, fmt.Errorf("invalid cluster %d", cluster)
	}

	return base + (uint64(cluster)-2)*clusterSize, nil
}

func clustersToRanges(
	clusters []uint32,
	clusterBase,
	clusterSize,
	fileLength uint64,
) []ByteRange {
	if len(clusters) == 0 {
		return nil
	}

	full := fileLength == 0
	remaining := fileLength
	ranges := make([]ByteRange, 0, 4)

	start := clusters[0]
	prev := start

	flush := func(a, b uint32) bool {
		runSize := (uint64(b) - uint64(a) + 1) * clusterSize
		if !full && runSize > remaining {
			runSize = remaining
		}

		startOff := clusterBase + (uint64(a)-2)*clusterSize
		ranges = append(ranges, ByteRange{
			Start: startOff,
			End:   startOff + runSize,
		})

		if !full {
			remaining -= runSize
			return remaining == 0
		}
		return false
	}

	for _, cluster := range clusters[1:] {
		if cluster == prev+1 {
			prev = cluster
			continue
		}

		if flush(start, prev) {
			return ranges
		}

		start = cluster
		prev = cluster
	}

	flush(start, prev)
	return ranges
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

type fatReader struct {
	fd *os.File

	baseOffset   uint64
	clusterCount uint64

	mask       uint32
	eoc        uint32
	bad        uint32
	rejectZero bool

	cache      [fatCacheSize]byte
	cacheBase  uint64
	cacheValid bool

	seen map[uint32]struct{}
}

func newFATReader(
	fd *os.File,
	baseOffset,
	clusterCount uint64,
	mask,
	eoc,
	bad uint32,
	rejectZero bool,
) fatReader {
	return fatReader{
		fd:           fd,
		baseOffset:   baseOffset,
		clusterCount: clusterCount,
		mask:         mask,
		eoc:          eoc,
		bad:          bad,
		rejectZero:   rejectZero,
		seen:         make(map[uint32]struct{}),
	}
}

func (f *fatReader) next(cluster uint32) (uint32, error) {
	off := f.baseOffset + uint64(cluster)*4
	base := off &^ uint64(fatCacheSize-1)

	if !f.cacheValid || f.cacheBase != base {
		if _, err := f.fd.ReadAt(f.cache[:], int64(base)); err != nil {
			return 0, err
		}
		f.cacheBase = base
		f.cacheValid = true
	}

	i := int(off - base)
	return binary.LittleEndian.Uint32(f.cache[i:i+4]) & f.mask, nil
}

func (f *fatReader) chain(first, maxClusters uint64, label string) ([]uint32, error) {
	if first < 2 {
		return nil, nil
	}

	chain := make([]uint32, 0, minU64(maxClusters, 1024))
	clear(f.seen)

	cur := uint32(first)

	for uint64(len(chain)) < maxClusters {
		if cur < 2 || uint64(cur) > f.clusterCount+1 {
			return nil, fmt.Errorf(
				"%s invalid FAT cluster %d",
				label,
				cur,
			)
		}

		if _, exists := f.seen[cur]; exists {
			return nil, fmt.Errorf(
				"%s FAT loop at cluster %d",
				label,
				cur,
			)
		}

		f.seen[cur] = struct{}{}
		chain = append(chain, cur)

		next, err := f.next(cur)
		if err != nil {
			return nil, err
		}

		if next >= f.eoc {
			break
		}

		if next == f.bad {
			return nil, fmt.Errorf(
				"%s bad cluster marker at %d",
				label,
				cur,
			)
		}

		if f.rejectZero && next == 0 {
			return nil, fmt.Errorf(
				"%s free cluster in chain at %d",
				label,
				cur,
			)
		}

		cur = next
	}

	return chain, nil
}

func sortRanges(ranges []ByteRange) {
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].Start == ranges[j].Start {
			return ranges[i].End < ranges[j].End
		}
		return ranges[i].Start < ranges[j].Start
	})
}
