package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"sort"
	"sync"
)

// NTFS is being used here as a disk-image format, not as a fully emulated
// filesystem. The FUSE layer still exposes the image byte-for-byte. This
// tracker answers one question only:
//
//   "Which physical bytes belong to an ordinary file's default $DATA stream?"
//
// It deliberately does NOT inspect file names. The MPEG-TS detector belongs
// to the byte stream path (DiskNode.broadcastTS), where the actual bytes are
// available.

const (
	ntfsMagic     = "NTFS    "
	ntfsFileMagic = "FILE"

	ntfsRootRecord = uint64(5)

	ntfsAttrData            = uint32(0x80)
	ntfsAttrIndexAllocation = uint32(0xA0)
	ntfsAttrBitmap          = uint32(0xB0)
	ntfsAttrEnd             = uint32(0xFFFFFFFF)

	ntfsFlagInUse     = uint16(0x0001)
	ntfsFlagDirectory = uint16(0x0002)

	ntfsAttrCompressed = uint16(0x0001)
	ntfsAttrEncrypted  = uint16(0x4000)
	ntfsAttrSparse     = uint16(0x8000)

	ntfsMaxRecordSize = uint64(1 << 20)

	// NTFS multi-sector protection uses a 512-byte sequence-number stride.
	// This is intentionally NOT the filesystem's physical bytes/sector value.
	ntfsUSNStride = uint64(512)

	// The first 16 MFT entries are NTFS metadata files, not user recording
	// files. Extension records belonging to a user file can have any later ID.
	ntfsFirstUserRecord = uint64(16)

	ntfsScanChunkSize = uint64(8 << 20)
)

type ntfsRun struct {
	StartVCN uint64
	Length   uint64
	LCN      int64
	Sparse   bool
}

type ntfsAttribute struct {
	Offset          int
	Type            uint32
	Length          uint32
	NonResident     bool
	Name            string
	Flags           uint16
	StartingVCN     uint64
	LastVCN         uint64
	MappingPairsOff uint16
	AllocatedSize   uint64
	RealSize        uint64
	ValueOff        uint16
	ValueLen        uint32
	Runs            []ntfsRun
}

type ntfsRecordInfo struct {
	ID            uint64
	BaseID        uint64
	InUse         bool
	IsDir         bool
	DataRanges    []ByteRange
	RootMetaRange []ByteRange
}

type NTFSTracker struct {
	partition Partition
	reader    io.ReaderAt

	bytesPerSector uint64
	clusterSize    uint64
	totalClusters  uint64
	mftRecordSize  uint64
	bootMFTLCN     uint64
	mftRuns        []ntfsRun
	mftSize        uint64

	mu sync.RWMutex

	records map[uint64]*ntfsRecordInfo

	// dataRanges are physical extents belonging to unnamed $DATA streams of
	// ordinary user files. These are only MPEG-TS *candidates*. The tracker
	// does not inspect filenames and does not claim that arbitrary file data
	// is transport stream data. The byte-level detector in DiskNode decides
	// whether a candidate is actually MPEG-TS.
	dataRanges []ByteRange

	rootRanges []ByteRange
}

var _ Tracker = (*NTFSTracker)(nil)

func NewNTFSTracker(part Partition, r io.ReaderAt) (*NTFSTracker, error) {
	if part.Offset < 0 {
		return nil, fmt.Errorf("invalid NTFS partition offset %d", part.Offset)
	}
	if part.Size <= 0 {
		return nil, fmt.Errorf("invalid NTFS partition size %d", part.Size)
	}

	t := &NTFSTracker{
		partition: part,
		reader:    r,
		records:   make(map[uint64]*ntfsRecordInfo),
	}

	if err := t.readBootSector(); err != nil {
		return nil, err
	}
	if err := t.loadMFTLayout(); err != nil {
		return nil, err
	}
	if err := t.scanMFT(); err != nil {
		return nil, err
	}

	return t, nil
}

// MetadataEnd cannot express NTFS correctly: NTFS metadata is distributed
// across the volume rather than living in one contiguous prefix.
//
// The existing FUSE Read() still accepts the old scalar MetadataEnd() API and
// zeroes bytes at/after that boundary. Returning MaxUint64 is the compatibility
// value that disables that obsolete zeroing rule for NTFS. The proper read path
// should classify the requested range instead.
func (t *NTFSTracker) MetadataEnd() uint64 {
	return ^uint64(0)
}

func (t *NTFSTracker) InRootDir(off uint64) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, r := range t.rootRanges {
		if off >= r.Start && off < r.End {
			return true
		}
		if r.Start > off {
			break
		}
	}
	return false
}

// Classify maps physical disk bytes to NTFS file-data extents.
//
// Anything outside an ordinary file's unnamed nonresident $DATA extent is
// metadata/other filesystem content from tssniff's perspective. No filename is
// consulted here. That means vendor-specific names, extensionless recordings,
// and arbitrary recorder file naming all behave the same.
func (t *NTFSTracker) Classify(start, length uint64) []ByteRange {
	if length == 0 {
		return nil
	}

	end := start + length
	if end < start {
		end = ^uint64(0)
	}

	t.mu.RLock()
	ranges := append([]ByteRange(nil), t.dataRanges...)
	t.mu.RUnlock()

	if len(ranges) == 0 {
		return []ByteRange{{Start: start, End: end, Kind: RangeMeta}}
	}

	out := make([]ByteRange, 0, 4)
	cursor := start

	idx := sort.Search(len(ranges), func(i int) bool {
		return ranges[i].End > start
	})

	for ; idx < len(ranges); idx++ {
		r := ranges[idx]
		if r.Start >= end {
			break
		}
		if r.End <= cursor {
			continue
		}

		if r.Start > cursor {
			b := minU64(r.Start, end)
			if cursor < b {
				out = append(out, ByteRange{
					Start: cursor,
					End:   b,
					Kind:  RangeMeta,
				})
			}
		}

		a := maxU64(cursor, r.Start)
		b := minU64(end, r.End)
		if a < b {
			out = append(out, ByteRange{
				Start:        a,
				End:          b,
				Kind:         RangeCandidate,
				Name:         r.Name,
				StreamID:     r.StreamID,
				StreamOffset: r.StreamOffset + (a - r.Start),
			})
			cursor = b
		}
	}

	if cursor < end {
		out = append(out, ByteRange{
			Start: cursor,
			End:   end,
			Kind:  RangeMeta,
		})
	}

	return coalesceClassifiedRanges(out)
}

func subtractNTSRanges(
	newRanges []ByteRange,
	oldRanges []ByteRange,
) []ByteRange {
	if len(newRanges) == 0 {
		return nil
	}

	out := make([]ByteRange, 0)

	for _, nr := range newRanges {
		pieces := []ByteRange{nr}

		idx := sort.Search(
			len(oldRanges),
			func(i int) bool {
				return oldRanges[i].End > nr.Start
			},
		)

		for ; idx < len(oldRanges); idx++ {
			or := oldRanges[idx]

			if or.Start >= nr.End {
				break
			}

			if or.End <= nr.Start {
				continue
			}

			if or.StreamID != nr.StreamID {
				continue
			}

			a := maxU64(nr.Start, or.Start)
			b := minU64(nr.End, or.End)

			if a >= b {
				continue
			}

			/*
				Physical overlap alone is not enough.

				Make sure both ranges map that physical location
				to the same logical stream offset.
			*/
			nLogical := nr.StreamOffset + (a - nr.Start)
			oLogical := or.StreamOffset + (a - or.Start)

			if nLogical != oLogical {
				continue
			}

			next := make([]ByteRange, 0, len(pieces)+1)

			for _, p := range pieces {
				if b <= p.Start || a >= p.End {
					next = append(next, p)
					continue
				}

				if p.Start < a {
					next = append(next, ByteRange{
						Start:        p.Start,
						End:          a,
						Kind:         p.Kind,
						Name:         p.Name,
						StreamID:     p.StreamID,
						StreamOffset: p.StreamOffset,
					})
				}

				if b < p.End {
					next = append(next, ByteRange{
						Start:        b,
						End:          p.End,
						Kind:         p.Kind,
						Name:         p.Name,
						StreamID:     p.StreamID,
						StreamOffset: p.StreamOffset + (b - p.Start),
					})
				}
			}

			pieces = next

			if len(pieces) == 0 {
				break
			}
		}

		out = append(out, pieces...)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].StreamID == out[j].StreamID {
			return out[i].StreamOffset < out[j].StreamOffset
		}
		return out[i].StreamID < out[j].StreamID
	})

	return coalesceNTSRanges(out)
}

func (t *NTFSTracker) OnMetadataWrite(start, length uint64) []ByteRange {
	if length == 0 {
		return nil
	}

	end := start + length
	if end <= start {
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	oldRanges := append([]ByteRange(nil), t.dataRanges...)

	ids, touchesRecordZero := t.mftRecordIDsForPhysicalRangeLocked(start, end)
	if !touchesRecordZero && len(ids) == 0 {
		return nil
	}

	oldMFTSize := t.mftSize
	changed := false

	if touchesRecordZero {
		if err := t.refreshRecordLocked(0); err != nil {
			if verbLog {
				log.Printf("NTFS: MFT record 0 refresh deferred: %v", err)
			}
		} else {
			changed = true
			ids[0] = struct{}{}
		}
	}

	if t.mftSize > oldMFTSize {
		if err := t.scanMFTLogicalRangeLocked(oldMFTSize, t.mftSize); err != nil {
			if verbLog {
				log.Printf("NTFS: scan newly grown MFT range failed: %v", err)
			}
		} else {
			changed = true
		}
	}

	for id := range ids {
		if id == 0 {
			continue
		}

		if id >= t.mftRecordCount() {
			delete(t.records, id)
			continue
		}

		if err := t.refreshRecordLocked(id); err != nil {
			if verbLog {
				log.Printf(
					"NTFS: MFT record %d refresh deferred: %v",
					id,
					err,
				)
			}
			continue
		}

		changed = true
	}

	count := t.mftRecordCount()

	for id := range t.records {
		if id >= count {
			delete(t.records, id)
			changed = true
		}
	}

	if !changed {
		return nil
	}

	t.rebuildRangesLocked()

	return subtractNTSRanges(
		t.dataRanges,
		oldRanges,
	)
}

func (t *NTFSTracker) readBootSector() error {
	var boot [512]byte
	if _, err := t.reader.ReadAt(boot[:], t.partition.Offset); err != nil {
		return fmt.Errorf("read NTFS boot sector: %w", err)
	}
	if string(boot[3:11]) != ntfsMagic {
		return fmt.Errorf("NTFS OEM ID not found")
	}
	if binary.LittleEndian.Uint16(boot[510:512]) != 0xAA55 {
		return fmt.Errorf("invalid NTFS boot signature")
	}

	bps := uint64(binary.LittleEndian.Uint16(boot[11:13]))
	spcByte := boot[13]
	if bps == 0 || spcByte == 0 {
		return fmt.Errorf("invalid NTFS sector/cluster geometry: bps=%d spc=%d", bps, spcByte)
	}
	if bps < 512 || bps > 4096 || (bps&(bps-1)) != 0 {
		return fmt.Errorf("unsupported NTFS bytes/sector %d", bps)
	}

	spc := uint64(spcByte)
	if spc > 128 || bps > ^uint64(0)/spc {
		return fmt.Errorf("unsupported NTFS sectors/cluster %d", spc)
	}

	t.clusterSize = bps * spc
	t.bytesPerSector = bps

	totalSectors := binary.LittleEndian.Uint64(boot[40:48])
	t.bootMFTLCN = binary.LittleEndian.Uint64(boot[48:56])

	recordSize, err := decodeNTFSRecordSize(int8(boot[64]), spc, bps)
	if err != nil {
		return fmt.Errorf("decode MFT record size: %w", err)
	}
	if recordSize < bps || recordSize > ntfsMaxRecordSize || recordSize%ntfsUSNStride != 0 {
		return fmt.Errorf("unsupported MFT record size %d", recordSize)
	}

	partClusters := uint64(t.partition.Size) / t.clusterSize
	if totalSectors != 0 {
		bootClusters := totalSectors / spc
		if bootClusters < partClusters {
			partClusters = bootClusters
		}
	}
	if partClusters == 0 {
		return fmt.Errorf("NTFS partition contains no clusters")
	}
	if t.bootMFTLCN >= partClusters {
		return fmt.Errorf("MFT LCN %d outside volume", t.bootMFTLCN)
	}

	t.totalClusters = partClusters
	t.mftRecordSize = recordSize
	return nil
}

func decodeNTFSRecordSize(v int8, sectorsPerCluster, bytesPerSector uint64) (uint64, error) {
	if v > 0 {
		if uint64(v) > ^uint64(0)/sectorsPerCluster {
			return 0, fmt.Errorf("MFT record cluster count overflow")
		}
		clusters := uint64(v) * sectorsPerCluster
		if clusters > ^uint64(0)/bytesPerSector {
			return 0, fmt.Errorf("MFT record size overflow")
		}
		return clusters * bytesPerSector, nil
	}

	shift := uint8(-v)
	if shift == 0 || shift >= 63 {
		return 0, fmt.Errorf("MFT record-size exponent too large: %d", shift)
	}
	return uint64(1) << shift, nil
}

func (t *NTFSTracker) loadMFTLayout() error {
	// $MFT record 0 starts at the LCN advertised by the boot sector. Its first
	// record is necessarily readable before we know the complete MFT runlist.
	off := uint64(t.partition.Offset) + t.bootMFTLCN*t.clusterSize
	buf, err := t.readRawRecordByOffset(off)
	if err != nil {
		return fmt.Errorf("read $MFT record 0: %w", err)
	}

	info, attrs, err := parseNTFSRecord(buf, 0)
	if err != nil {
		return fmt.Errorf("parse $MFT record 0: %w", err)
	}
	if !info.InUse {
		return fmt.Errorf("$MFT record 0 is not in use")
	}

	var data *ntfsAttribute
	for i := range attrs {
		a := &attrs[i]
		if a.Type == ntfsAttrData && a.Name == "" && a.NonResident {
			data = a
			break
		}
	}
	if data == nil {
		return fmt.Errorf("$MFT has no unnamed nonresident $DATA")
	}
	if data.Flags&(ntfsAttrCompressed|ntfsAttrEncrypted|ntfsAttrSparse) != 0 {
		return fmt.Errorf("$MFT $DATA has unsupported flags %#x", data.Flags)
	}

	t.mftRuns = append([]ntfsRun(nil), data.Runs...)
	t.mftSize = data.RealSize
	if t.mftSize == 0 {
		t.mftSize = data.AllocatedSize
	}
	if t.mftSize == 0 {
		return fmt.Errorf("$MFT has zero size")
	}

	return nil
}

func parseNTFSRunlist(data []byte, startVCN uint64) ([]ntfsRun, error) {
	var runs []ntfsRun
	var currentLCN int64
	vcn := startVCN

	for i := 0; i < len(data); {
		header := data[i]
		i++

		if header == 0 {
			return runs, nil
		}

		lengthSize := int(header & 0x0f)
		offsetSize := int(header >> 4)
		if lengthSize == 0 || lengthSize > 8 || offsetSize > 8 {
			return nil, fmt.Errorf("invalid NTFS runlist header %#x", header)
		}
		if i+lengthSize+offsetSize > len(data) {
			return nil, io.ErrUnexpectedEOF
		}

		var runLength uint64
		for j := 0; j < lengthSize; j++ {
			runLength |= uint64(data[i+j]) << uint(8*j)
		}
		i += lengthSize
		if runLength == 0 {
			return nil, fmt.Errorf("zero-length NTFS run")
		}

		if offsetSize == 0 {
			runs = append(runs, ntfsRun{
				StartVCN: vcn,
				Length:   runLength,
				LCN:      -1,
				Sparse:   true,
			})
		} else {
			var raw uint64
			for j := 0; j < offsetSize; j++ {
				raw |= uint64(data[i+j]) << uint(8*j)
			}
			i += offsetSize

			bits := uint(offsetSize * 8)
			var delta int64
			if bits == 64 {
				delta = int64(raw)
			} else {
				value := int64(raw & ((uint64(1) << bits) - 1))
				if raw&(uint64(1)<<(bits-1)) != 0 {
					value -= int64(uint64(1) << bits)
				}
				delta = value
			}

			if delta > 0 && currentLCN > int64(^uint64(0)>>1)-delta {
				return nil, fmt.Errorf("NTFS LCN overflow")
			}
			if delta < 0 && currentLCN < -int64(^uint64(0)>>1)-1-delta {
				return nil, fmt.Errorf("NTFS LCN underflow")
			}
			currentLCN += delta
			if currentLCN < 0 {
				return nil, fmt.Errorf("negative NTFS LCN %d", currentLCN)
			}

			runs = append(runs, ntfsRun{
				StartVCN: vcn,
				Length:   runLength,
				LCN:      currentLCN,
			})
		}

		if vcn > ^uint64(0)-runLength {
			return nil, fmt.Errorf("NTFS VCN overflow")
		}
		vcn += runLength
	}

	return nil, io.ErrUnexpectedEOF
}

func applyNTFSFixup(record []byte, _ uint64) error {
	if len(record) == 0 || uint64(len(record))%ntfsUSNStride != 0 || len(record) < 8 {
		return fmt.Errorf("invalid NTFS record geometry")
	}

	usaOff := int(binary.LittleEndian.Uint16(record[4:6]))
	usaCount := int(binary.LittleEndian.Uint16(record[6:8]))
	sectors := len(record) / int(ntfsUSNStride)
	if usaOff < 8 || usaCount != sectors+1 || usaOff+usaCount*2 > len(record) {
		return fmt.Errorf("invalid NTFS update-sequence array")
	}

	usn := record[usaOff : usaOff+2]
	for i := 0; i < sectors; i++ {
		trailer := int(ntfsUSNStride)*(i+1) - 2
		if record[trailer] != usn[0] || record[trailer+1] != usn[1] {
			return fmt.Errorf("NTFS fixup mismatch at sector %d", i)
		}

		repl := record[usaOff+2+2*i : usaOff+4+2*i]
		record[trailer] = repl[0]
		record[trailer+1] = repl[1]
	}

	return nil
}

func parseNTFSAttribute(record []byte, off, limit int) (ntfsAttribute, error) {
	var a ntfsAttribute
	a.Offset = off

	if off < 0 || off+16 > limit || off+16 > len(record) {
		return a, io.ErrUnexpectedEOF
	}

	a.Type = binary.LittleEndian.Uint32(record[off : off+4])
	if a.Type == ntfsAttrEnd {
		return a, nil
	}

	a.Length = binary.LittleEndian.Uint32(record[off+4 : off+8])
	if a.Length < 0x18 || a.Length%8 != 0 {
		return a, fmt.Errorf("invalid NTFS attribute length %d", a.Length)
	}

	attrEnd := off + int(a.Length)
	if attrEnd < off || attrEnd > limit || attrEnd > len(record) {
		return a, io.ErrUnexpectedEOF
	}

	a.NonResident = record[off+8] != 0
	nameLen := int(record[off+9])
	nameOff := int(binary.LittleEndian.Uint16(record[off+10 : off+12]))
	a.Flags = binary.LittleEndian.Uint16(record[off+12 : off+14])

	if nameLen > 0 {
		nameStart := off + nameOff
		nameEnd := nameStart + nameLen*2
		if nameOff < 0 || nameStart < off || nameEnd > attrEnd {
			return a, fmt.Errorf("invalid NTFS attribute name")
		}
		// We deliberately do not decode or inspect the name. For this tracker,
		// an unnamed $DATA attribute is the default file stream and named data
		// streams are simply not candidates.
		a.Name = "<named>"
	}

	if !a.NonResident {
		if off+24 > attrEnd {
			return a, io.ErrUnexpectedEOF
		}
		a.ValueLen = binary.LittleEndian.Uint32(record[off+16 : off+20])
		a.ValueOff = binary.LittleEndian.Uint16(record[off+20 : off+22])
		valueStart := off + int(a.ValueOff)
		valueEnd := valueStart + int(a.ValueLen)
		if valueStart < off || valueEnd < valueStart || valueEnd > attrEnd {
			return a, fmt.Errorf("invalid resident NTFS attribute value")
		}
		return a, nil
	}

	if off+0x40 > attrEnd {
		return a, io.ErrUnexpectedEOF
	}

	a.StartingVCN = binary.LittleEndian.Uint64(record[off+0x10 : off+0x18])
	a.LastVCN = binary.LittleEndian.Uint64(record[off+0x18 : off+0x20])
	a.MappingPairsOff = binary.LittleEndian.Uint16(record[off+0x20 : off+0x22])
	a.AllocatedSize = binary.LittleEndian.Uint64(record[off+0x28 : off+0x30])
	a.RealSize = binary.LittleEndian.Uint64(record[off+0x30 : off+0x38])

	pairsStart := off + int(a.MappingPairsOff)
	if pairsStart < off || pairsStart > attrEnd {
		return a, fmt.Errorf("invalid NTFS mapping-pairs offset")
	}

	if pairsStart == attrEnd {
		if a.RealSize != 0 {
			return a, fmt.Errorf("nonresident NTFS attribute has no mapping pairs")
		}
		return a, nil
	}

	runs, err := parseNTFSRunlist(record[pairsStart:attrEnd], a.StartingVCN)
	if err != nil {
		return a, fmt.Errorf("parse NTFS data runs: %w", err)
	}
	if len(runs) == 0 && a.RealSize != 0 {
		return a, fmt.Errorf("nonresident NTFS attribute has empty runlist")
	}
	if len(runs) > 0 && a.LastVCN < a.StartingVCN {
		return a, fmt.Errorf("invalid NTFS VCN range")
	}

	a.Runs = runs
	return a, nil
}

func parseNTFSRecord(buf []byte, id uint64) (*ntfsRecordInfo, []ntfsAttribute, error) {
	if len(buf) < 48 || string(buf[:4]) != ntfsFileMagic {
		return nil, nil, fmt.Errorf("invalid NTFS FILE record")
	}

	flags := binary.LittleEndian.Uint16(buf[22:24])
	baseRef := binary.LittleEndian.Uint64(buf[32:40]) & 0x0000FFFFFFFFFFFF
	if baseRef == 0 {
		baseRef = id
	}

	info := &ntfsRecordInfo{
		ID:     id,
		BaseID: baseRef,
		InUse:  flags&ntfsFlagInUse != 0,
		IsDir:  flags&ntfsFlagDirectory != 0,
	}
	if !info.InUse {
		return info, nil, nil
	}

	firstAttr := int(binary.LittleEndian.Uint16(buf[20:22]))
	if firstAttr < 0x18 || firstAttr >= len(buf) {
		return nil, nil, fmt.Errorf("invalid first attribute offset %d", firstAttr)
	}

	attrs := make([]ntfsAttribute, 0, 8)
	for off := firstAttr; off+4 <= len(buf); {
		if binary.LittleEndian.Uint32(buf[off:off+4]) == ntfsAttrEnd {
			break
		}

		a, err := parseNTFSAttribute(buf, off, len(buf))
		if err != nil {
			return nil, nil, err
		}
		if a.Length == 0 {
			return nil, nil, fmt.Errorf("zero-length NTFS attribute")
		}
		attrs = append(attrs, a)
		off += int(a.Length)
	}

	return info, attrs, nil
}

func (t *NTFSTracker) readRawRecordByOffset(off uint64) ([]byte, error) {
	if t.mftRecordSize == 0 || t.mftRecordSize > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("invalid MFT record size %d", t.mftRecordSize)
	}

	buf := make([]byte, int(t.mftRecordSize))
	if _, err := t.reader.ReadAt(buf, int64(off)); err != nil {
		return nil, err
	}
	if err := applyNTFSFixup(buf, t.bytesPerSector); err != nil {
		return nil, err
	}
	if string(buf[:4]) != ntfsFileMagic {
		return nil, fmt.Errorf("invalid MFT record magic")
	}
	return buf, nil
}

// readMFTLogical reads from the logical byte stream represented by $MFT's
// runlist. This is deliberately separate from physical offsets so fragmented
// MFT storage is handled correctly.
func (t *NTFSTracker) readMFTLogical(logical uint64, dst []byte) error {
	if len(dst) == 0 {
		return nil
	}
	if logical > t.mftSize || uint64(len(dst)) > t.mftSize-logical {
		return fmt.Errorf("MFT logical read outside $MFT size")
	}

	remaining := uint64(len(dst))
	logicalPos := logical
	dstPos := uint64(0)

	for remaining > 0 {
		mapped := false

		for _, run := range t.mftRuns {
			if run.Sparse || run.LCN < 0 {
				continue
			}

			runStart := run.StartVCN * t.clusterSize
			runBytes := run.Length * t.clusterSize
			if runBytes == 0 || logicalPos < runStart {
				continue
			}
			if logicalPos >= runStart+runBytes {
				continue
			}

			within := logicalPos - runStart
			available := runBytes - within
			if available > remaining {
				available = remaining
			}

			phys := uint64(t.partition.Offset) + uint64(run.LCN)*t.clusterSize + within
			if available > uint64(^uint(0)>>1) {
				return fmt.Errorf("MFT physical read too large")
			}
			if _, err := t.reader.ReadAt(dst[dstPos:dstPos+available], int64(phys)); err != nil {
				return err
			}

			logicalPos += available
			dstPos += available
			remaining -= available
			mapped = true
			break
		}

		if !mapped {
			return fmt.Errorf("MFT logical offset %d is not mapped", logicalPos)
		}
	}

	return nil
}

func (t *NTFSTracker) readRecord(id uint64) (*ntfsRecordInfo, []ntfsAttribute, error) {
	if id >= t.mftRecordCount() {
		return nil, nil, fmt.Errorf("MFT record %d outside $MFT", id)
	}

	logical := id * t.mftRecordSize
	buf := make([]byte, int(t.mftRecordSize))
	if err := t.readMFTLogical(logical, buf); err != nil {
		return nil, nil, err
	}
	if err := applyNTFSFixup(buf, t.bytesPerSector); err != nil {
		return nil, nil, err
	}

	return parseNTFSRecord(buf, id)
}

func (t *NTFSTracker) mftRecordIDsForPhysicalRangeLocked(start, end uint64) (map[uint64]struct{}, bool) {
	ids := make(map[uint64]struct{})
	touchesRecordZero := false

	for _, run := range t.mftRuns {
		if run.Sparse || run.LCN < 0 {
			continue
		}

		if run.Length > ^uint64(0)/t.clusterSize {
			continue
		}
		runBytes := run.Length * t.clusterSize
		physStart := uint64(t.partition.Offset) + uint64(run.LCN)*t.clusterSize
		physEnd := physStart + runBytes
		if physEnd < physStart || start >= physEnd || end <= physStart {
			continue
		}

		a := maxU64(start, physStart)
		b := minU64(end, physEnd)
		if a >= b {
			continue
		}

		logicalA := run.StartVCN*t.clusterSize + (a - physStart)
		logicalB := run.StartVCN*t.clusterSize + (b - physStart)
		if logicalB <= logicalA {
			continue
		}

		first := logicalA / t.mftRecordSize
		last := (logicalB - 1) / t.mftRecordSize
		for id := first; id <= last; id++ {
			if id == 0 {
				touchesRecordZero = true
			}
			ids[id] = struct{}{}
			if id == ^uint64(0) {
				break
			}
		}
	}

	return ids, touchesRecordZero
}

// physicalRangesForMFTLogical maps a logical $MFT region back to physical
// disk ranges. This is useful for root-directory reset detection and keeps
// fragmentation out of the rest of the tracker.
func (t *NTFSTracker) physicalRangesForMFTLogical(logical, length uint64, kind RangeKind) []ByteRange {
	if length == 0 || logical > t.mftSize || length > t.mftSize-logical {
		return nil
	}

	end := logical + length
	out := make([]ByteRange, 0, 2)

	for _, run := range t.mftRuns {
		if run.Sparse || run.LCN < 0 {
			continue
		}

		runStart := run.StartVCN * t.clusterSize
		runBytes := run.Length * t.clusterSize
		runEnd := runStart + runBytes
		if runBytes == 0 || runEnd < runStart || runStart >= end || runEnd <= logical {
			continue
		}

		a := maxU64(logical, runStart)
		b := minU64(end, runEnd)
		if a >= b {
			continue
		}

		within := a - runStart
		physStart := uint64(t.partition.Offset) + uint64(run.LCN)*t.clusterSize + within
		out = append(out, ByteRange{
			Start: physStart,
			End:   physStart + (b - a),
			Kind:  kind,
		})
	}

	return out
}

func (t *NTFSTracker) refreshRecordLocked(id uint64) error {
	info, attrs, err := t.readRecord(id)
	if err != nil {
		return err
	}

	if id == 0 {
		var data *ntfsAttribute
		for i := range attrs {
			a := &attrs[i]
			if a.Type == ntfsAttrData && a.Name == "" && a.NonResident {
				data = a
				break
			}
		}
		if data == nil {
			return fmt.Errorf("$MFT has no unnamed nonresident $DATA")
		}
		if data.Flags&(ntfsAttrCompressed|ntfsAttrEncrypted|ntfsAttrSparse) != 0 {
			return fmt.Errorf("$MFT $DATA has unsupported flags %#x", data.Flags)
		}

		newSize := data.RealSize
		if newSize == 0 {
			newSize = data.AllocatedSize
		}
		if newSize == 0 {
			return fmt.Errorf("$MFT has zero size")
		}

		t.mftRuns = append([]ntfsRun(nil), data.Runs...)
		t.mftSize = newSize
	}

	if !info.InUse {
		delete(t.records, id)
		return nil
	}

	t.buildDataRanges(info, attrs)
	t.buildRootRanges(info, attrs)
	t.records[id] = info
	return nil
}

func (t *NTFSTracker) buildDataRanges(info *ntfsRecordInfo, attrs []ntfsAttribute) {
	info.DataRanges = nil

	// Only user files are candidates. The first 16 records are NTFS special
	// files. Extension records are accepted when their BaseID points to a user
	// file; rebuildRangesLocked verifies that the base record is live and a
	// regular file before exposing the ranges.
	if info.BaseID < ntfsFirstUserRecord || info.IsDir {
		return
	}

	streamID := fmt.Sprintf("ntfs:%d", info.BaseID)
	partStart := uint64(t.partition.Offset)
	partEnd := partStart + uint64(t.partition.Size)

	for _, a := range attrs {
		if a.Type != ntfsAttrData || a.Name != "" || !a.NonResident {
			continue
		}
		if a.Flags&(ntfsAttrCompressed|ntfsAttrEncrypted|ntfsAttrSparse) != 0 {
			continue
		}

		for _, run := range a.Runs {
			if run.Sparse || run.LCN < 0 || uint64(run.LCN) >= t.totalClusters {
				continue
			}
			if run.Length > ^uint64(0)/t.clusterSize {
				continue
			}
			if run.StartVCN > ^uint64(0)/t.clusterSize {
				continue
			}

			logicalStart := run.StartVCN * t.clusterSize
			lengthBytes := run.Length * t.clusterSize
			if lengthBytes == 0 || logicalStart >= a.RealSize {
				continue
			}

			remaining := a.RealSize - logicalStart
			if lengthBytes > remaining {
				lengthBytes = remaining
			}

			if uint64(run.LCN) > (^uint64(0)-partStart)/t.clusterSize {
				continue
			}
			start := partStart + uint64(run.LCN)*t.clusterSize
			if start >= partEnd || lengthBytes > partEnd-start {
				continue
			}
			end := start + lengthBytes

			info.DataRanges = append(info.DataRanges, ByteRange{
				Start:        start,
				End:          end,
				Kind:         RangeCandidate,
				Name:         streamID,
				StreamID:     streamID,
				StreamOffset: logicalStart,
			})
		}
	}
}

func (t *NTFSTracker) buildRootRanges(info *ntfsRecordInfo, attrs []ntfsAttribute) {
	info.RootMetaRange = nil

	if info.ID != ntfsRootRecord || !info.InUse {
		return
	}

	// The root record itself contains its resident $INDEX_ROOT and other
	// directory bookkeeping, so mark its physical MFT record as metadata.
	if ranges := t.physicalRangesForMFTLogical(
		ntfsRootRecord*t.mftRecordSize,
		t.mftRecordSize,
		RangeMeta,
	); len(ranges) > 0 {
		info.RootMetaRange = append(info.RootMetaRange, ranges...)
	}

	// Large directories keep their index on disk in a named $I30 stream.
	// This is metadata, not recording payload.
	for _, a := range attrs {
		if (a.Type != ntfsAttrIndexAllocation && a.Type != ntfsAttrBitmap) ||
			a.Name != "<named>" || !a.NonResident {
			continue
		}
		if a.Flags&(ntfsAttrCompressed|ntfsAttrEncrypted|ntfsAttrSparse) != 0 {
			continue
		}

		for _, run := range a.Runs {
			if run.Sparse || run.LCN < 0 || uint64(run.LCN) >= t.totalClusters {
				continue
			}
			if run.Length > ^uint64(0)/t.clusterSize {
				continue
			}

			start := uint64(t.partition.Offset) + uint64(run.LCN)*t.clusterSize
			length := run.Length * t.clusterSize
			partEnd := uint64(t.partition.Offset) + uint64(t.partition.Size)
			if start >= partEnd || length > partEnd-start {
				continue
			}

			info.RootMetaRange = append(info.RootMetaRange, ByteRange{
				Start: start,
				End:   start + length,
				Kind:  RangeMeta,
			})
		}
	}
}

func (t *NTFSTracker) scanMFT() error {
	count := t.mftRecordCount()
	if count == 0 {
		return fmt.Errorf("$MFT contains no file records")
	}

	partCapacity := uint64(t.partition.Size) / t.mftRecordSize
	if count > partCapacity {
		count = partCapacity
	}
	if count == 0 {
		return fmt.Errorf("partition cannot contain an MFT record")
	}

	t.mftSize = minU64(t.mftSize, count*t.mftRecordSize)
	if t.mftSize < t.mftRecordSize {
		return fmt.Errorf("$MFT is smaller than one record")
	}

	if err := t.scanMFTLogicalRangeLocked(0, t.mftSize); err != nil {
		return err
	}

	t.rebuildRangesLocked()
	return nil
}

// scanMFTLogicalRangeLocked scans records in a logical $MFT interval. The
// caller holds t.mu when using this during a live refresh; initial startup is
// single-threaded and safe as well.
func (t *NTFSTracker) scanMFTLogicalRangeLocked(logicalStart, logicalEnd uint64) error {
	if logicalStart > logicalEnd || logicalEnd > t.mftSize {
		return fmt.Errorf("invalid MFT scan range %#x-%#x", logicalStart, logicalEnd)
	}

	logicalStart -= logicalStart % t.mftRecordSize
	logicalEnd -= logicalEnd % t.mftRecordSize
	if logicalEnd <= logicalStart {
		return nil
	}

	count := t.mftRecordCount()
	partCapacity := uint64(t.partition.Size) / t.mftRecordSize
	if count > partCapacity {
		count = partCapacity
	}

	for logical := logicalStart; logical < logicalEnd; {
		chunk := minU64(ntfsScanChunkSize, logicalEnd-logical)
		chunk -= chunk % t.mftRecordSize
		if chunk == 0 {
			break
		}

		buf := make([]byte, int(chunk))
		if err := t.readMFTLogical(logical, buf); err != nil {
			return fmt.Errorf("read MFT logical range %#x-%#x: %w", logical, logical+chunk, err)
		}

		for pos := uint64(0); pos < chunk; pos += t.mftRecordSize {
			id := (logical + pos) / t.mftRecordSize
			if id >= count {
				break
			}

			recStart := int(pos)
			recEnd := recStart + int(t.mftRecordSize)
			rec := buf[recStart:recEnd]

			if err := applyNTFSFixup(rec, t.bytesPerSector); err != nil {
				continue
			}
			if string(rec[:4]) != ntfsFileMagic {
				continue
			}

			info, attrs, err := parseNTFSRecord(rec, id)
			if err != nil {
				continue
			}
			if !info.InUse {
				delete(t.records, id)
				continue
			}

			t.buildDataRanges(info, attrs)
			t.buildRootRanges(info, attrs)
			t.records[id] = info
		}

		logical += chunk
		if verbLog {
			current := logical / t.mftRecordSize
			log.Printf("NTFS: scanned MFT record %d/%d", minU64(current, count), count)
		}
	}

	return nil
}

func (t *NTFSTracker) mftRecordCount() uint64 {
	if t.mftRecordSize == 0 || t.mftSize == 0 {
		return 0
	}
	return (t.mftSize + t.mftRecordSize - 1) / t.mftRecordSize
}

func (t *NTFSTracker) rebuildRangesLocked() {
	activeFiles := make(map[uint64]struct{})

	for id, info := range t.records {
		// A base record identifies the file. Only ordinary, in-use files are
		// allowed to activate their own and extension records' data ranges.
		if !info.InUse || info.IsDir || id != info.BaseID || id < ntfsFirstUserRecord {
			continue
		}
		activeFiles[id] = struct{}{}
	}

	data := make([]ByteRange, 0)
	root := make([]ByteRange, 0)

	for _, info := range t.records {
		if _, ok := activeFiles[info.BaseID]; ok {
			data = append(data, info.DataRanges...)
		}
		root = append(root, info.RootMetaRange...)
	}

	sort.Slice(data, func(i, j int) bool {
		if data[i].Start == data[j].Start {
			if data[i].End == data[j].End {
				if data[i].StreamID == data[j].StreamID {
					return data[i].StreamOffset < data[j].StreamOffset
				}
				return data[i].StreamID < data[j].StreamID
			}
			return data[i].End < data[j].End
		}
		return data[i].Start < data[j].Start
	})
	data = coalesceNTSRanges(data)

	sortRanges(root)
	root = coalesceRanges(root)

	t.dataRanges = data
	t.rootRanges = root
}

func coalesceClassifiedRanges(in []ByteRange) []ByteRange {
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

			canMerge := last.End >= r.Start &&
				last.Kind == r.Kind &&
				last.Name == r.Name &&
				last.StreamID == r.StreamID

			if canMerge && r.Kind == RangeCandidate {
				lastLen := last.End - last.Start

				if last.StreamOffset+lastLen != r.StreamOffset {
					canMerge = false
				}
			}

			if canMerge {
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

func coalesceNTSRanges(in []ByteRange) []ByteRange {
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
			lastLen := last.End - last.Start

			if last.End == r.Start &&
				last.Kind == r.Kind &&
				last.StreamID == r.StreamID &&
				last.StreamOffset+lastLen == r.StreamOffset {
				last.End = r.End
				continue
			}
		}

		out = append(out, r)
	}

	return out
}
