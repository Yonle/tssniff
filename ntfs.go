package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"sort"
	"sync/atomic"
)

const (
	ntfsMagic     = "NTFS    "
	ntfsFileMagic = "FILE"

	ntfsAttrData = uint32(0x80)
	ntfsAttrEnd  = uint32(0xFFFFFFFF)

	ntfsFlagInUse     = uint16(0x0001)
	ntfsFlagDirectory = uint16(0x0002)

	ntfsAttrCompressed = uint16(0x0001)
	ntfsAttrEncrypted  = uint16(0x4000)
	ntfsAttrSparse     = uint16(0x8000)

	ntfsMaxRecordSize = uint64(1 << 20)

	ntfsFirstUserRecord = uint64(16)

	ntfsScanChunkSize = uint64(8 << 20)
)

type NTFSRun struct {
	StartVCN uint64
	Length   uint64
	LCN      int64
	Sparse   bool
}

type NTFSAttribute struct {
	Type        uint32
	Length      uint32
	NonResident bool
	Name        string
	Flags       uint16

	StartingVCN uint64
	LastVCN     uint64

	AllocatedSize uint64
	RealSize      uint64

	Runs []NTFSRun
}

type NTFSRecord struct {
	ID       uint64
	BaseID   uint64
	Sequence uint16

	InUse bool
	IsDir bool

	DataRanges []ByteRange
}

type RangeSnapshot struct {
	Ranges []ByteRange
}

type NTFS struct {
	partition Partition

	reader io.ReaderAt

	bytesPerSector uint64
	clusterSize    uint64
	totalClusters  uint64

	mftRecordSize uint64
	bootMFTLCN    uint64

	mftRuns []NTFSRun
	mftSize uint64

	/*
		Only NTFS worker code changes these.
	*/
	records map[uint64]*NTFSRecord

	dataRanges []ByteRange
	mftRanges  []ByteRange

	/*
		Read concurrently by FUSE Write().

		The pointed-to snapshots are immutable after publication.
	*/
	mftSnapshot  atomic.Pointer[RangeSnapshot]
	dataSnapshot atomic.Pointer[RangeSnapshot]
}

func NewNTFS(
	part Partition,
	reader io.ReaderAt,
) (*NTFS, error) {
	if part.Offset < 0 {
		return nil, fmt.Errorf(
			"invalid NTFS partition offset %d",
			part.Offset,
		)
	}

	if part.Size <= 0 {
		return nil, fmt.Errorf(
			"invalid NTFS partition size %d",
			part.Size,
		)
	}

	n := &NTFS{
		partition: part,
		reader:    reader,
		records:   make(map[uint64]*NTFSRecord),
	}

	if err := n.readBootSector(); err != nil {
		return nil, err
	}

	if err := n.loadMFTLayout(); err != nil {
		return nil, err
	}

	if err := n.scanMFT(); err != nil {
		return nil, err
	}

	n.publishSnapshots()

	return n, nil
}

func (n *NTFS) TouchesMFT(
	start,
	end uint64,
) bool {
	snapshot := n.mftSnapshot.Load()

	if snapshot == nil {
		return false
	}

	return rangesOverlapSnapshot(
		snapshot.Ranges,
		start,
		end,
	)
}

func (n *NTFS) DataRanges() []ByteRange {
	snapshot := n.dataSnapshot.Load()

	if snapshot == nil {
		return nil
	}

	return snapshot.Ranges
}

func rangesOverlapSnapshot(
	ranges []ByteRange,
	start,
	end uint64,
) bool {
	if end <= start ||
		len(ranges) == 0 {
		return false
	}

	idx := sort.Search(
		len(ranges),
		func(i int) bool {
			return ranges[i].End > start
		},
	)

	return idx < len(ranges) &&
		ranges[idx].Start < end
}

func (n *NTFS) ObserveMFTWrite(
	start,
	end uint64,
) []ByteRange {
	if end <= start {
		return nil
	}

	oldData := append(
		[]ByteRange(nil),
		n.dataRanges...,
	)

	oldMFTSize := n.mftSize

	ids,
		touchesRecordZero := n.mftRecordIDsForPhysicalRange(
		start,
		end,
	)

	/*
		Record 0 controls the $MFT runlist.

		Refresh it first when touched.
	*/
	if touchesRecordZero {
		if err := n.refreshRecord(0); err != nil {
			if verbLog {
				log.Printf(
					"NTFS: MFT record 0 refresh deferred: %v",
					err,
				)
			}
		} else {
			/*
				The runlist may have changed, so recompute what
				the current physical write maps to.
			*/
			newIDs,
				newZero := n.mftRecordIDsForPhysicalRange(
				start,
				end,
			)

			for id := range rangeKey(newIDs) {
				ids[uint64(id)] = struct{}{}
			}

			if newZero {
				touchesRecordZero = true
			}
		}
	}

	/*
		If $MFT grew, scan only the new logical tail.

		No full-volume replay.
	*/
	if n.mftSize > oldMFTSize {
		scanStart :=
			oldMFTSize -
				oldMFTSize%n.mftRecordSize

		if err := n.scanMFTLogicalRange(
			scanStart,
			n.mftSize,
		); err != nil {
			if verbLog {
				log.Printf(
					"NTFS: new MFT range scan deferred: %v",
					err,
				)
			}
		}
	}

	for id := range ids {
		if id == 0 {
			continue
		}

		if id >= n.mftRecordCount() {
			delete(
				n.records,
				id,
			)

			continue
		}

		if err := n.refreshRecord(id); err != nil {

			if verbLog {
				log.Printf(
					"NTFS: MFT record %d refresh deferred: %v",
					id,
					err,
				)
			}

			continue
		}
	}

	n.rebuildRanges()

	n.publishSnapshots()

	return subtractRanges(
		n.dataRanges,
		oldData,
	)
}

func rangeKey(
	m map[uint64]struct{},
) []uint64 {
	if len(m) == 0 {
		return nil
	}

	out := make(
		[]uint64,
		0,
		len(m),
	)

	for id := range m {
		out = append(out, id)
	}

	return out
}

func (n *NTFS) readBootSector() error {
	var boot [512]byte

	if _, err := n.reader.ReadAt(
		boot[:],
		n.partition.Offset,
	); err != nil {
		return fmt.Errorf(
			"read NTFS boot sector: %w",
			err,
		)
	}

	if string(boot[3:11]) != ntfsMagic {
		return fmt.Errorf(
			"NTFS OEM ID not found",
		)
	}

	if binary.LittleEndian.Uint16(
		boot[510:512],
	) != 0xAA55 {
		return fmt.Errorf(
			"invalid NTFS boot signature",
		)
	}

	bps := uint64(
		binary.LittleEndian.Uint16(
			boot[11:13],
		),
	)

	spcByte := boot[13]

	if bps == 0 ||
		spcByte == 0 {
		return fmt.Errorf(
			"invalid NTFS geometry",
		)
	}

	if bps < 512 ||
		bps > 4096 ||
		bps&(bps-1) != 0 {
		return fmt.Errorf(
			"unsupported NTFS bytes/sector %d",
			bps,
		)
	}

	spc := uint64(spcByte)

	if bps >
		^uint64(0)/spc {
		return fmt.Errorf(
			"NTFS cluster size overflow",
		)
	}

	n.bytesPerSector = bps
	n.clusterSize = bps * spc

	totalSectors := binary.LittleEndian.Uint64(
		boot[40:48],
	)

	n.bootMFTLCN = binary.LittleEndian.Uint64(
		boot[48:56],
	)

	recordSize,
		err := decodeNTFSRecordSize(
		int8(boot[64]),
		spc,
		bps,
	)
	if err != nil {
		return err
	}

	if recordSize < bps ||
		recordSize > ntfsMaxRecordSize ||
		recordSize%bps != 0 {
		return fmt.Errorf(
			"unsupported MFT record size %d",
			recordSize,
		)
	}

	partClusters :=
		uint64(n.partition.Size) /
			n.clusterSize

	if totalSectors != 0 {
		bootClusters :=
			totalSectors / spc

		if bootClusters < partClusters {
			partClusters = bootClusters
		}
	}

	if partClusters == 0 ||
		n.bootMFTLCN >= partClusters {
		return fmt.Errorf(
			"invalid NTFS MFT location",
		)
	}

	n.totalClusters = partClusters
	n.mftRecordSize = recordSize

	return nil
}

func decodeNTFSRecordSize(
	v int8,
	sectorsPerCluster,
	bytesPerSector uint64,
) (uint64, error) {
	if v > 0 {
		clusters :=
			uint64(v) *
				sectorsPerCluster

		return clusters *
				bytesPerSector,
			nil
	}

	shift := uint8(-v)

	if shift == 0 ||
		shift >= 63 {
		return 0,
			fmt.Errorf(
				"invalid MFT record exponent",
			)
	}

	return uint64(1) << shift,
		nil
}

func (n *NTFS) loadMFTLayout() error {
	off :=
		uint64(n.partition.Offset) +
			n.bootMFTLCN*
				n.clusterSize

	buf,
		err := n.readRawRecord(
		off,
	)
	if err != nil {
		return fmt.Errorf(
			"read MFT record 0: %w",
			err,
		)
	}

	_, attrs,
		err := parseNTFSRecord(
		buf,
		0,
	)
	if err != nil {
		return fmt.Errorf(
			"parse MFT record 0: %w",
			err,
		)
	}

	for _, a := range attrs {
		if a.Type != ntfsAttrData ||
			a.Name != "" ||
			!a.NonResident {
			continue
		}

		if a.Flags&(ntfsAttrCompressed|
			ntfsAttrEncrypted|
			ntfsAttrSparse) != 0 {
			continue
		}

		n.mftRuns = append(
			[]NTFSRun(nil),
			a.Runs...,
		)

		n.mftSize = a.RealSize

		if n.mftSize == 0 {
			n.mftSize = a.AllocatedSize
		}

		if n.mftSize == 0 {
			return fmt.Errorf(
				"MFT has zero size",
			)
		}

		return nil
	}

	return fmt.Errorf(
		"MFT has no unnamed nonresident DATA",
	)
}

func parseNTFSRunlist(
	data []byte,
	startVCN uint64,
) ([]NTFSRun, error) {
	var runs []NTFSRun

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

		if lengthSize == 0 ||
			lengthSize > 8 ||
			offsetSize > 8 {
			return nil,
				fmt.Errorf(
					"invalid NTFS runlist header %#x",
					header,
				)
		}

		if i+
			lengthSize+
			offsetSize >
			len(data) {

			return nil,
				io.ErrUnexpectedEOF
		}

		var runLength uint64

		for j := 0; j < lengthSize; j++ {
			runLength |=
				uint64(data[i+j]) <<
					uint(8*j)
		}

		i += lengthSize

		if runLength == 0 {
			return nil,
				fmt.Errorf(
					"zero-length NTFS run",
				)
		}

		if offsetSize == 0 {
			runs = append(
				runs,
				NTFSRun{
					StartVCN: vcn,
					Length:   runLength,
					LCN:      -1,
					Sparse:   true,
				},
			)
		} else {
			var raw uint64

			for j := 0; j < offsetSize; j++ {
				raw |=
					uint64(data[i+j]) <<
						uint(8*j)
			}

			i += offsetSize

			bits := uint(offsetSize * 8)

			var delta int64

			if bits == 64 {
				delta = int64(raw)
			} else {
				value := int64(
					raw &
						((uint64(1) << bits) - 1),
				)

				if raw&
					(uint64(1)<<(bits-1)) != 0 {
					value -= int64(
						uint64(1) << bits,
					)
				}

				delta = value
			}

			currentLCN += delta

			if currentLCN < 0 {
				return nil,
					fmt.Errorf(
						"negative NTFS LCN",
					)
			}

			runs = append(
				runs,
				NTFSRun{
					StartVCN: vcn,
					Length:   runLength,
					LCN:      currentLCN,
				},
			)
		}

		vcn += runLength
	}

	return nil,
		io.ErrUnexpectedEOF
}

func applyNTFSFixup(
	record []byte,
	bytesPerSector uint64,
) error {
	if bytesPerSector == 0 {
		return fmt.Errorf(
			"invalid sector size",
		)
	}

	stride := int(bytesPerSector)

	if len(record) == 0 ||
		len(record)%stride != 0 ||
		len(record) < 8 {
		return fmt.Errorf(
			"invalid NTFS record geometry",
		)
	}

	usaOff := int(
		binary.LittleEndian.Uint16(
			record[4:6],
		),
	)

	usaCount := int(
		binary.LittleEndian.Uint16(
			record[6:8],
		),
	)

	sectors :=
		len(record) / stride

	if usaOff < 8 ||
		usaCount != sectors+1 ||
		usaOff+usaCount*2 >
			len(record) {
		return fmt.Errorf(
			"invalid NTFS update-sequence array",
		)
	}

	usn := record[usaOff : usaOff+2]

	for i := 0; i < sectors; i++ {
		trailer :=
			stride*(i+1) - 2

		if record[trailer] != usn[0] ||
			record[trailer+1] != usn[1] {

			return fmt.Errorf(
				"NTFS fixup mismatch at sector %d",
				i,
			)
		}

		repl := record[usaOff+2+2*i : usaOff+4+2*i]

		record[trailer] = repl[0]
		record[trailer+1] = repl[1]
	}

	return nil
}

func parseNTFSAttribute(
	record []byte,
	off,
	limit int,
) (NTFSAttribute, error) {
	var a NTFSAttribute

	if off < 0 ||
		off+16 > limit ||
		off+16 > len(record) {
		return a,
			io.ErrUnexpectedEOF
	}

	a.Type = binary.LittleEndian.Uint32(
		record[off : off+4],
	)

	if a.Type == ntfsAttrEnd {
		return a, nil
	}

	a.Length = binary.LittleEndian.Uint32(
		record[off+4 : off+8],
	)

	if a.Length < 0x18 ||
		a.Length%8 != 0 {
		return a,
			fmt.Errorf(
				"invalid NTFS attribute length %d",
				a.Length,
			)
	}

	attrEnd :=
		off +
			int(a.Length)

	if attrEnd > limit ||
		attrEnd > len(record) {
		return a,
			io.ErrUnexpectedEOF
	}

	a.NonResident =
		record[off+8] != 0

	nameLen := int(record[off+9])

	if nameLen > 0 {
		nameOff := int(
			binary.LittleEndian.Uint16(
				record[off+10 : off+12],
			),
		)

		nameStart :=
			off +
				nameOff

		nameEnd :=
			nameStart +
				nameLen*2

		if nameStart < off ||
			nameEnd > attrEnd {
			return a,
				fmt.Errorf(
					"invalid NTFS attribute name",
				)
		}

		a.Name = "<named>"
	}

	a.Flags = binary.LittleEndian.Uint16(
		record[off+12 : off+14],
	)

	if !a.NonResident {
		return a, nil
	}

	if off+0x40 > attrEnd {
		return a,
			io.ErrUnexpectedEOF
	}

	a.StartingVCN = binary.LittleEndian.Uint64(
		record[off+0x10 : off+0x18],
	)

	a.LastVCN = binary.LittleEndian.Uint64(
		record[off+0x18 : off+0x20],
	)

	mappingPairsOff := int(
		binary.LittleEndian.Uint16(
			record[off+0x20 : off+0x22],
		),
	)

	a.AllocatedSize = binary.LittleEndian.Uint64(
		record[off+0x28 : off+0x30],
	)

	a.RealSize = binary.LittleEndian.Uint64(
		record[off+0x30 : off+0x38],
	)

	pairsStart :=
		off +
			mappingPairsOff

	if pairsStart < off ||
		pairsStart > attrEnd {
		return a,
			fmt.Errorf(
				"invalid mapping-pairs offset",
			)
	}

	if pairsStart == attrEnd {
		return a, nil
	}

	runs,
		err := parseNTFSRunlist(
		record[pairsStart:attrEnd],
		a.StartingVCN,
	)
	if err != nil {
		return a,
			err
	}

	a.Runs = runs

	return a, nil
}

func parseNTFSRecord(
	buf []byte,
	id uint64,
) (*NTFSRecord, []NTFSAttribute, error) {
	if len(buf) < 48 ||
		string(buf[:4]) != ntfsFileMagic {
		return nil, nil,
			fmt.Errorf(
				"invalid NTFS FILE record",
			)
	}

	info := &NTFSRecord{
		ID: id,

		BaseID: binary.LittleEndian.Uint64(
			buf[32:40],
		) &
			0x0000FFFFFFFFFFFF,

		Sequence: binary.LittleEndian.Uint16(
			buf[16:18],
		),

		InUse: binary.LittleEndian.Uint16(
			buf[22:24],
		)&ntfsFlagInUse != 0,

		IsDir: binary.LittleEndian.Uint16(
			buf[22:24],
		)&ntfsFlagDirectory != 0,
	}

	if info.BaseID == 0 {
		info.BaseID = id
	}

	if !info.InUse {
		return info, nil, nil
	}

	firstAttr := int(
		binary.LittleEndian.Uint16(
			buf[20:22],
		),
	)

	if firstAttr < 0x18 ||
		firstAttr >= len(buf) {
		return nil, nil,
			fmt.Errorf(
				"invalid first attribute offset",
			)
	}

	var attrs []NTFSAttribute

	for off := firstAttr; off+4 <= len(buf); {

		if binary.LittleEndian.Uint32(
			buf[off:off+4],
		) == ntfsAttrEnd {
			break
		}

		a, err := parseNTFSAttribute(
			buf,
			off,
			len(buf),
		)
		if err != nil {
			return nil, nil, err
		}

		if a.Length == 0 {
			return nil, nil,
				fmt.Errorf(
					"zero-length NTFS attribute",
				)
		}

		attrs = append(attrs, a)

		off += int(a.Length)
	}

	return info, attrs, nil
}

func (n *NTFS) readRawRecord(
	physical uint64,
) ([]byte, error) {
	buf := make(
		[]byte,
		int(n.mftRecordSize),
	)

	if _, err := n.reader.ReadAt(
		buf,
		int64(physical),
	); err != nil {
		return nil, err
	}

	if err := applyNTFSFixup(
		buf,
		n.bytesPerSector,
	); err != nil {
		return nil, err
	}

	return buf, nil
}

func (n *NTFS) readMFTLogical(
	logical uint64,
	dst []byte,
) error {
	if logical > n.mftSize ||
		uint64(len(dst)) >
			n.mftSize-logical {
		return fmt.Errorf(
			"MFT logical read outside volume",
		)
	}

	remaining := uint64(len(dst))

	pos := logical

	dstPos := uint64(0)

	for remaining > 0 {
		mapped := false

		for _, run := range n.mftRuns {
			if run.Sparse ||
				run.LCN < 0 {
				continue
			}

			runStart :=
				run.StartVCN *
					n.clusterSize

			runBytes :=
				run.Length *
					n.clusterSize

			runEnd :=
				runStart +
					runBytes

			if pos < runStart ||
				pos >= runEnd {
				continue
			}

			within :=
				pos -
					runStart

			nbytes :=
				runBytes -
					within

			if nbytes > remaining {
				nbytes = remaining
			}

			physical :=
				uint64(n.partition.Offset) +
					uint64(run.LCN)*
						n.clusterSize +
					within

			readDst := dst[int(dstPos):int(dstPos+nbytes)]

			if _, err := n.reader.ReadAt(
				readDst,
				int64(physical),
			); err != nil {
				return err
			}

			pos += nbytes
			dstPos += nbytes
			remaining -= nbytes

			mapped = true

			break
		}

		if !mapped {
			return fmt.Errorf(
				"MFT logical offset %d is unmapped",
				pos,
			)
		}
	}

	return nil
}

func (n *NTFS) readRecord(
	id uint64,
) (*NTFSRecord, []NTFSAttribute, error) {
	if id >= n.mftRecordCount() {
		return nil, nil,
			fmt.Errorf(
				"MFT record %d outside MFT",
				id,
			)
	}

	logical :=
		id *
			n.mftRecordSize

	buf := make(
		[]byte,
		int(n.mftRecordSize),
	)

	if err := n.readMFTLogical(
		logical,
		buf,
	); err != nil {
		return nil, nil, err
	}

	if err := applyNTFSFixup(
		buf,
		n.bytesPerSector,
	); err != nil {
		return nil, nil, err
	}

	return parseNTFSRecord(
		buf,
		id,
	)
}

func (n *NTFS) mftRecordIDsForPhysicalRange(
	start,
	end uint64,
) (map[uint64]struct{}, bool) {
	ids := make(
		map[uint64]struct{},
	)

	touchesZero := false

	for _, run := range n.mftRuns {
		if run.Sparse ||
			run.LCN < 0 {
			continue
		}

		physStart :=
			uint64(n.partition.Offset) +
				uint64(run.LCN)*
					n.clusterSize

		physEnd :=
			physStart +
				run.Length*
					n.clusterSize

		if start >= physEnd ||
			end <= physStart {
			continue
		}

		a := maxU64(
			start,
			physStart,
		)

		b := minU64(
			end,
			physEnd,
		)

		logicalA :=
			run.StartVCN*
				n.clusterSize +
				(a - physStart)

		logicalB :=
			run.StartVCN*
				n.clusterSize +
				(b - physStart)

		first :=
			logicalA /
				n.mftRecordSize

		last :=
			(logicalB - 1) /
				n.mftRecordSize

		for id := first; id <= last; id++ {

			ids[id] = struct{}{}

			if id == 0 {
				touchesZero = true
			}

			if id == ^uint64(0) {
				break
			}
		}
	}

	return ids, touchesZero
}

func (n *NTFS) refreshRecord(
	id uint64,
) error {
	info,
		attrs,
		err := n.readRecord(id)
	if err != nil {
		return err
	}

	if id == 0 {
		for _, a := range attrs {
			if a.Type != ntfsAttrData ||
				a.Name != "" ||
				!a.NonResident {
				continue
			}

			if a.Flags&(ntfsAttrCompressed|
				ntfsAttrEncrypted|
				ntfsAttrSparse) != 0 {
				continue
			}

			n.mftRuns = append(
				[]NTFSRun(nil),
				a.Runs...,
			)

			n.mftSize = a.RealSize

			if n.mftSize == 0 {
				n.mftSize = a.AllocatedSize
			}

			break
		}
	}

	if !info.InUse {
		delete(
			n.records,
			id,
		)

		return nil
	}

	n.buildDataRanges(
		info,
		attrs,
	)

	n.records[id] = info

	return nil
}

func (n *NTFS) buildDataRanges(
	info *NTFSRecord,
	attrs []NTFSAttribute,
) {
	info.DataRanges = nil

	if info.BaseID < ntfsFirstUserRecord ||
		info.IsDir {
		return
	}

	partStart := uint64(n.partition.Offset)

	partEnd :=
		partStart +
			uint64(n.partition.Size)

	for _, a := range attrs {
		if a.Type != ntfsAttrData ||
			a.Name != "" ||
			!a.NonResident {
			continue
		}

		if a.Flags&(ntfsAttrCompressed|
			ntfsAttrEncrypted|
			ntfsAttrSparse) != 0 {
			continue
		}

		for _, run := range a.Runs {
			if run.Sparse ||
				run.LCN < 0 ||
				uint64(run.LCN) >= n.totalClusters {
				continue
			}

			logicalStart :=
				run.StartVCN *
					n.clusterSize

			lengthBytes :=
				run.Length *
					n.clusterSize

			if logicalStart >= a.RealSize {
				continue
			}

			remaining :=
				a.RealSize -
					logicalStart

			if lengthBytes >
				remaining {
				lengthBytes = remaining
			}

			physicalStart :=
				partStart +
					uint64(run.LCN)*
						n.clusterSize

			if physicalStart >= partEnd ||
				lengthBytes >
					partEnd-physicalStart {
				continue
			}

			info.DataRanges = append(
				info.DataRanges,
				ByteRange{
					Start: physicalStart,
					End:   physicalStart + lengthBytes,
				},
			)
		}
	}
}

func (n *NTFS) rebuildRanges() {
	var data []ByteRange

	activeFiles := make(
		map[uint64]struct{},
	)

	for id, info := range n.records {
		if id != info.BaseID ||
			id < ntfsFirstUserRecord ||
			!info.InUse ||
			info.IsDir {
			continue
		}

		activeFiles[id] = struct{}{}
	}

	for _, info := range n.records {
		if _, ok := activeFiles[info.BaseID]; !ok {
			continue
		}

		data = append(
			data,
			info.DataRanges...,
		)
	}

	sort.Slice(
		data,
		func(i, j int) bool {
			if data[i].Start != data[j].Start {
				return data[i].Start <
					data[j].Start
			}

			return data[i].End <
				data[j].End
		},
	)

	n.dataRanges = coalesceByteRanges(data)

	n.mftRanges = n.buildMFTRanges()
}

func (n *NTFS) buildMFTRanges() []ByteRange {
	if n.mftSize == 0 {
		return nil
	}

	var out []ByteRange

	remaining := n.mftSize

	logical := uint64(0)

	for _, run := range n.mftRuns {
		if remaining == 0 {
			break
		}

		runBytes :=
			run.Length *
				n.clusterSize

		if runBytes == 0 {
			continue
		}

		nbytes := runBytes

		if nbytes > remaining {
			nbytes = remaining
		}

		if !run.Sparse &&
			run.LCN >= 0 {

			start :=
				uint64(n.partition.Offset) +
					uint64(run.LCN)*
						n.clusterSize

			out = append(
				out,
				ByteRange{
					Start: start,
					End:   start + nbytes,
				},
			)
		}

		logical += nbytes
		remaining -= nbytes
	}

	_ = logical

	return coalesceByteRanges(out)
}

func (n *NTFS) publishSnapshots() {
	data := &RangeSnapshot{
		Ranges: append(
			[]ByteRange(nil),
			n.dataRanges...,
		),
	}

	mft := &RangeSnapshot{
		Ranges: append(
			[]ByteRange(nil),
			n.mftRanges...,
		),
	}

	n.dataSnapshot.Store(data)
	n.mftSnapshot.Store(mft)
}

func (n *NTFS) scanMFT() error {
	count := n.mftRecordCount()

	if count == 0 {
		return fmt.Errorf(
			"empty MFT",
		)
	}

	return n.scanMFTLogicalRange(
		0,
		n.mftSize,
	)
}

func (n *NTFS) scanMFTLogicalRange(
	start,
	end uint64,
) error {
	start -=
		start %
			n.mftRecordSize

	end -=
		end %
			n.mftRecordSize

	if end <= start {
		return nil
	}

	for logical := start; logical < end; {

		chunk := minU64(
			ntfsScanChunkSize,
			end-logical,
		)

		chunk -=
			chunk %
				n.mftRecordSize

		if chunk == 0 {
			break
		}

		buf := make(
			[]byte,
			int(chunk),
		)

		if err := n.readMFTLogical(
			logical,
			buf,
		); err != nil {
			return err
		}

		for pos := uint64(0); pos < chunk; pos += n.mftRecordSize {

			id :=
				(logical + pos) /
					n.mftRecordSize

			rec := buf[int(pos):int(pos+n.mftRecordSize)]

			if err := applyNTFSFixup(
				rec,
				n.bytesPerSector,
			); err != nil {
				continue
			}

			if string(rec[:4]) != ntfsFileMagic {
				continue
			}

			info,
				attrs,
				err := parseNTFSRecord(
				rec,
				id,
			)
			if err != nil {
				continue
			}

			if !info.InUse {
				delete(
					n.records,
					id,
				)

				continue
			}

			n.buildDataRanges(
				info,
				attrs,
			)

			n.records[id] = info
		}

		logical += chunk
	}

	n.rebuildRanges()

	return nil
}

func (n *NTFS) mftRecordCount() uint64 {
	if n.mftRecordSize == 0 ||
		n.mftSize == 0 {
		return 0
	}

	return (n.mftSize + n.mftRecordSize - 1) / n.mftRecordSize
}

func subtractRanges(
	newRanges,
	oldRanges []ByteRange,
) []ByteRange {
	if len(newRanges) == 0 {
		return nil
	}

	var out []ByteRange

	j := 0

	for _, nr := range newRanges {
		if nr.End <= nr.Start {
			continue
		}

		for j < len(oldRanges) &&
			oldRanges[j].End <= nr.Start {
			j++
		}

		cursor := nr.Start

		for k := j; k < len(oldRanges) &&
			oldRanges[k].Start < nr.End; k++ {

			or := oldRanges[k]

			if or.End <= cursor {
				continue
			}

			if or.Start > cursor {
				out = append(
					out,
					ByteRange{
						Start: cursor,
						End: minU64(
							or.Start,
							nr.End,
						),
					},
				)
			}

			if or.End > cursor {
				cursor = or.End
			}

			if cursor >= nr.End {
				break
			}
		}

		if cursor < nr.End {
			out = append(
				out,
				ByteRange{
					Start: cursor,
					End:   nr.End,
				},
			)
		}
	}

	return coalesceByteRanges(out)
}

func coalesceByteRanges(
	in []ByteRange,
) []ByteRange {
	if len(in) == 0 {
		return nil
	}

	sort.Slice(
		in,
		func(i, j int) bool {
			if in[i].Start != in[j].Start {
				return in[i].Start <
					in[j].Start
			}

			return in[i].End <
				in[j].End
		},
	)

	out := make(
		[]ByteRange,
		0,
		len(in),
	)

	for _, r := range in {
		if r.End <= r.Start {
			continue
		}

		if len(out) > 0 {
			last := &out[len(out)-1]

			if r.Start <= last.End {
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
