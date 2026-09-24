package main

import (
	"context"
	"io"
	"log"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type diskWriteJob struct {
	id   uint64
	seg  ByteRange
	data []byte

	barrier chan error // nil for normal writes
}

type cachedWrite struct {
	id    uint64
	start uint64
	data  []byte
}

type DiskNode struct {
	fs.Inode

	imgFile  *os.File
	size     uint64
	hub      *Hub
	tracker  Tracker
	preserve bool

	detector TSDetector

	tsMu sync.Mutex

	activeTSStream string

	// Async storage path.
	writeQ chan diskWriteJob

	cacheMu     sync.RWMutex
	cacheNextID uint64
	cache       []cachedWrite

	workerMu  sync.RWMutex
	workerErr error
}

var (
	_ fs.NodeGetattrer = (*DiskNode)(nil)
	_ fs.NodeOpener    = (*DiskNode)(nil)
	_ fs.NodeReader    = (*DiskNode)(nil)
	_ fs.NodeWriter    = (*DiskNode)(nil)
	_ fs.NodeFlusher   = (*DiskNode)(nil)
	_ fs.NodeFsyncer   = (*DiskNode)(nil)
)

func (d *DiskNode) Getattr(
	ctx context.Context,
	fh fs.FileHandle,
	out *fuse.AttrOut,
) syscall.Errno {
	out.Mode = syscall.S_IFREG | 0644
	out.Size = d.size
	out.Blksize = 512
	out.Blocks = d.size / 512

	return 0
}

func (d *DiskNode) Open(
	ctx context.Context,
	flags uint32,
) (fs.FileHandle, uint32, syscall.Errno) {
	return nil, fuse.FOPEN_DIRECT_IO, 0
}

func (d *DiskNode) Read(
	ctx context.Context,
	fh fs.FileHandle,
	dest []byte,
	off int64,
) (fuse.ReadResult, syscall.Errno) {
	if off < 0 {
		return fuse.ReadResultData(nil), syscall.EINVAL
	}

	start := uint64(off)

	if start >= d.size || len(dest) == 0 {
		return fuse.ReadResultData(nil), 0
	}

	length := uint64(len(dest))
	if length > d.size-start {
		length = d.size - start
	}

	data := dest[:int(length)]

	n, err := d.readBacking(data, off)
	if err != nil && err != io.EOF {
		return fuse.ReadResultData(nil), toErrno(err)
	}

	if n < len(data) {
		clear(data[n:])
	}

	if verbLog {
		log.Printf(
			"read off=%d len=%d",
			start,
			length,
		)
	}

	return fuse.ReadResultData(data), 0
}

func (d *DiskNode) Write(
	ctx context.Context,
	fh fs.FileHandle,
	data []byte,
	off int64,
) (uint32, syscall.Errno) {
	if off < 0 {
		return 0, syscall.EINVAL
	}

	start := uint64(off)

	if uint64(len(data)) > ^uint64(0)-start {
		return 0, syscall.EFBIG
	}

	end := start + uint64(len(data))

	if end > d.size {
		return 0, syscall.EFBIG
	}

	if len(data) == 0 {
		return 0, 0
	}

	/*
		If the asynchronous worker has already failed, stop accepting
		new writes. Previous writes were already ACKed, so we cannot
		report their failure retroactively.
	*/
	d.workerMu.RLock()
	workerErr := d.workerErr
	d.workerMu.RUnlock()

	if workerErr != nil {
		log.Printf(
			"write worker previously failed: %v",
			workerErr,
		)

		return 0, syscall.EIO
	}

	segs := d.tracker.Classify(
		start,
		uint64(len(data)),
	)

	if verbLog {
		log.Printf(
			"write off=%d len=%d segs=%d",
			start,
			len(data),
			len(segs),
		)
	}

	for _, seg := range segs {
		if seg.End <= seg.Start {
			continue
		}

		if seg.Start < start ||
			seg.End > end {
			log.Printf(
				"invalid tracker range: write=[%d,%d) seg=[%d,%d)",
				start,
				end,
				seg.Start,
				seg.End,
			)

			return 0, syscall.EIO
		}

		rel := seg.Start - start
		partLen := seg.End - seg.Start

		part := data[int(rel):int(rel+partLen)]

		/*
			Copy the bytes NOW.

			FUSE owns the incoming data buffer only for the duration
			of this Write() call. The background worker must therefore
			own its own copy.
		*/
		id := d.stageWrite(
			seg.Start,
			part,
		)

		jobData := append(
			[]byte(nil),
			part...,
		)

		job := diskWriteJob{
			id:   id,
			seg:  seg,
			data: jobData,
		}

		select {
		case d.writeQ <- job:
			/*
				Queued successfully.

				THIS is where Write() now stops waiting for
				WriteAt()/NTFS/replay/etc.
			*/

		case <-ctx.Done():
			d.removeCachedWrite(id)

			return 0, syscall.EINTR
		}
	}

	/*
		All data has been copied and queued.

		ACK the USB/FUSE write immediately.
	*/
	return uint32(len(data)), 0
}

/*
feedCandidate serializes all live candidate data against NTFS replay.

Once an MPEG-TS stream has actually been detected, another logical NTFS
stream is ignored so an old recording cannot splice itself into the current
one.

Before detection, another stream may replace the current tentative stream.
This allows a newly created recording to win if the previous candidate was
not actually MPEG-TS.
*/
func (d *DiskNode) feedCandidate(
	streamID string,
	logicalOffset uint64,
	data []byte,
) {
	if len(data) == 0 {
		return
	}

	d.tsMu.Lock()
	defer d.tsMu.Unlock()

	d.feedCandidateLocked(
		streamID,
		logicalOffset,
		data,
	)
}

func (d *DiskNode) feedCandidateLocked(
	streamID string,
	logicalOffset uint64,
	data []byte,
) {
	if len(data) == 0 {
		return
	}

	if streamID == "" {
		streamID = "default"
	}

	/*
		An already detected stream owns the TS pipeline.

		Other NTFS files may be RangeCandidate too, but they must not
		steal the active stream.
	*/
	if d.detector.detected &&
		d.activeTSStream != "" &&
		streamID != d.activeTSStream {
		if verbLog {
			log.Printf(
				"TS pipeline: ignore stream=%q active=%q logical=%d",
				streamID,
				d.activeTSStream,
				logicalOffset,
			)
		}
		return
	}

	/*
		Before detection, switching to another candidate is allowed.

		This is useful when one ordinary file is examined first and a
		different file turns out to be the actual recording.
	*/
	if d.activeTSStream != streamID {
		d.detector.Reset()
		d.activeTSStream = streamID

		if verbLog {
			log.Printf(
				"TS pipeline: switch stream=%q",
				streamID,
			)
		}
	}

	d.detector.Feed(
		streamID,
		logicalOffset,
		data,
		func(payload []byte) {
			if verbLog {
				log.Printf(
					"MPEG-TS broadcast len=%d",
					len(payload),
				)
			}

			d.hub.Broadcast(payload)
		},
	)

	if d.detector.detected {
		d.activeTSStream = streamID
	}
}

/*
processMetadataWrite serializes NTFS metadata refresh and any corresponding
candidate replay against live TS candidate feeding.

For FAT32 this reduces to the existing metadata callback.

For NTFS:

	metadata write
	    -> refresh MFT
	    -> discover newly visible candidate ranges
	    -> replay from sparse backing image
	    -> optionally punch those temporary bytes into holes
*/
func (d *DiskNode) processMetadataWrite(
	start,
	length uint64,
) syscall.Errno {
	d.tsMu.Lock()
	defer d.tsMu.Unlock()

	if d.tracker.InRootDir(start) {
		if verbLog {
			log.Printf(
				"root directory write at %d: reset TS detector",
				start,
			)
		}

		d.detector.Reset()
		d.activeTSStream = ""
	}

	d.tracker.OnMetadataWrite(
		start,
		length,
	)

	/*
		Keep the Tracker interface unchanged.

		The NTFS implementation exposes its delayed-discovery queue
		through a concrete method because FAT32 does not need this
		mechanism.
	*/
	ntfs, ok := d.tracker.(*NTFSTracker)
	if !ok {
		return 0
	}

	ranges, newStreams := ntfs.DrainDiscovery()

	if len(ranges) == 0 {
		return 0
	}

	return d.replayCandidateRangesLocked(
		ranges,
		newStreams,
	)
}

/*
replayCandidateRangesLocked must be called with tsMu held.

NTFS may learn about a file-data extent only after the original FUSE write has
already completed. The bytes were temporarily persisted into the sparse
backing image because they could not yet be classified.

Once the MFT reveals the extent, replay the saved bytes through the normal
TS detector.

newStreams identifies file streams which are genuinely new/reused MFT file
instances. Their first useful extent does not have to start at logical offset
zero, so they explicitly get a detector rollover.
*/
func (d *DiskNode) replayCandidateRangesLocked(
	ranges []ByteRange,
	newStreams map[string]struct{},
) syscall.Errno {
	if len(ranges) == 0 {
		return 0
	}

	ordered := append([]ByteRange(nil), ranges...)

	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].StreamID != ordered[j].StreamID {
			return ordered[i].StreamID < ordered[j].StreamID
		}
		if ordered[i].StreamOffset != ordered[j].StreamOffset {
			return ordered[i].StreamOffset < ordered[j].StreamOffset
		}
		return ordered[i].Start < ordered[j].Start
	})

	/*
		Try genuinely new streams first.

		If one of them becomes MPEG-TS, it becomes the active recording
		and other old streams are ignored.

		If it does NOT become MPEG-TS, continue to the next new stream
		or eventually fall back to already-existing streams.
	*/
	newIDs := make([]string, 0, len(newStreams))
	for streamID := range newStreams {
		newIDs = append(newIDs, streamID)
	}

	sort.Strings(newIDs)

	processedNew := make(map[string]struct{}, len(newIDs))

	for _, streamID := range newIDs {
		processedNew[streamID] = struct{}{}

		oldStream := d.activeTSStream

		d.detector.Reset()
		d.activeTSStream = streamID

		if verbLog {
			log.Printf(
				"TS pipeline: new recording stream=%q replacing=%q",
				streamID,
				oldStream,
			)
		}

		for _, r := range ordered {
			if r.StreamID != streamID {
				continue
			}

			if err := d.replayCandidateRangeLocked(r); err != 0 {
				return err
			}
		}

		/*
			If the initial discovered extent is too small to detect MPEG-TS,
			keep this stream active. Later writes from the same file will
			continue feeding the detector.
		*/
		if d.detector.detected {
			break
		}
	}

	/*
		Process remaining candidate ranges.

		If a new stream was detected, unrelated streams are still punched
		when preserve=false but do not need to be read and fed into the TS
		detector.

		If no new stream was detected, existing candidate streams are still
		allowed to feed the detector and may become the active stream.
	*/
	for _, r := range ordered {
		streamID := r.StreamID
		if streamID == "" {
			streamID = "default"
		}

		if _, isNew := newStreams[streamID]; isNew {
			if _, alreadyProcessed := processedNew[streamID]; alreadyProcessed {
				continue
			}

			/*
				Another newly discovered stream already became MPEG-TS.

				This one is no longer relevant to the live broadcast,
				but when preserve=false its temporary bytes must still
				be removed from the sparse backing image.
			*/
			if !d.preserve {
				if err := d.punchHole(
					r.Start,
					r.End-r.Start,
				); err != 0 {
					return err
				}
			}

			continue
		}

		/*
			If an actual TS stream is already locked, old/unrelated
			files should not be replayed into it.

			They still need suppression when preserve=false.
		*/
		if d.detector.detected &&
			d.activeTSStream != "" &&
			streamID != d.activeTSStream {
			if !d.preserve {
				if err := d.punchHole(
					r.Start,
					r.End-r.Start,
				); err != 0 {
					return err
				}
			}

			continue
		}

		if err := d.replayCandidateRangeLocked(r); err != 0 {
			return err
		}
	}

	return 0
}

/*
replayCandidateRangeLocked re-reads a newly discovered candidate extent from
the sparse backing image and feeds it into the TS pipeline.

The diagnostic log is deliberately before the range validation checks so it
shows exactly what the NTFS tracker returned.
*/
func (d *DiskNode) replayCandidateRangeLocked(
	r ByteRange,
) syscall.Errno {
	if verbLog {
		log.Printf(
			"NTFS replay candidate stream=%q logical=%d phys=[%d,%d)",
			r.StreamID,
			r.StreamOffset,
			r.Start,
			r.End,
		)
	}

	if r.End <= r.Start {
		return 0
	}

	if r.Kind != RangeCandidate {
		return 0
	}

	streamID := r.StreamID
	if streamID == "" {
		streamID = "default"
	}

	const replayChunkSize = 1 << 20 // 1 MiB

	remaining := r.End - r.Start
	phys := r.Start
	logical := r.StreamOffset

	for remaining > 0 {
		n := uint64(replayChunkSize)
		if n > remaining {
			n = remaining
		}

		buf := make([]byte, int(n))

		readN, err := d.imgFile.ReadAt(
			buf,
			int64(phys),
		)

		if err != nil && err != io.EOF {
			log.Printf(
				"NTFS replay read failed phys=%d len=%d: %v",
				phys,
				n,
				err,
			)
			return toErrno(err)
		}

		if readN == 0 {
			break
		}

		if verbLog {
			log.Printf(
				"NTFS replay phys=%d logical=%d len=%d stream=%q",
				phys,
				logical,
				readN,
				streamID,
			)
		}

		d.feedCandidateLocked(
			streamID,
			logical,
			buf[:readN],
		)

		phys += uint64(readN)
		logical += uint64(readN)
		remaining -= uint64(readN)

		if readN < int(n) {
			break
		}
	}

	/*
		The bytes existed only because NTFS had not yet told us that
		they were recording data.

		Once replayed, preserve=false restores the intended fake-storage
		behavior by deallocating the backing-file blocks. Linux documents
		that reads from a punched range subsequently return zeroes.
	*/
	if !d.preserve {
		if err := d.punchHole(
			r.Start,
			r.End-r.Start,
		); err != 0 {
			return err
		}
	}

	return 0
}

/*
Linux fallocate flags:

	FALLOC_FL_KEEP_SIZE = 0x01
	FALLOC_FL_PUNCH_HOLE = 0x02

FALLOC_FL_PUNCH_HOLE must be combined with KEEP_SIZE.
*/
const (
	fallocKeepSize  uint32 = 0x01
	fallocPunchHole uint32 = 0x02
)

/*
punchHole removes the physical blocks backing a byte range while retaining the
logical size of the sparse image.

Partial underlying filesystem blocks are zeroed; full blocks are deallocated.
*/
func (d *DiskNode) punchHole(
	off,
	length uint64,
) syscall.Errno {
	if length == 0 {
		return 0
	}

	err := syscall.Fallocate(
		int(d.imgFile.Fd()),
		fallocPunchHole|fallocKeepSize,
		int64(off),
		int64(length),
	)
	if err != nil {
		log.Printf(
			"punch hole failed off=%d len=%d: %v",
			off,
			length,
			err,
		)

		return toErrno(err)
	}

	if verbLog {
		log.Printf(
			"punched hole off=%d len=%d",
			off,
			length,
		)
	}

	return 0
}

func (d *DiskNode) writeBacking(
	data []byte,
	off uint64,
	kind string,
) syscall.Errno {
	start := time.Now()

	n, err := d.imgFile.WriteAt(
		data,
		int64(off),
	)

	delay := time.Since(start)

	if delay > 20*time.Millisecond {
		log.Printf(
			"slow %s write off=%d len=%d took=%v",
			kind,
			off,
			len(data),
			delay,
		)
	}

	if err != nil {
		return toErrno(err)
	}

	if n != len(data) {
		return syscall.EIO
	}

	return 0
}

func (d *DiskNode) Flush(
	ctx context.Context,
	fh fs.FileHandle,
) syscall.Errno {
	return d._sync(ctx)
}

func (d *DiskNode) Fsync(
	ctx context.Context,
	fh fs.FileHandle,
	flags uint32,
) syscall.Errno {
	return d._sync(ctx)
}

func (d *DiskNode) waitWriteQueue() error {
	done := make(chan error, 1)

	d.writeQ <- diskWriteJob{
		barrier: done,
	}

	return <-done
}

func (d *DiskNode) _sync(
	ctx context.Context,
) syscall.Errno {
	start := time.Now()

	err := d.waitWriteQueue()

	delay := time.Since(start)

	if delay > 20*time.Millisecond {
		log.Printf(
			"slow queued sync took=%v",
			delay,
		)
	}

	return toErrno(err)
}

func (d *DiskNode) processWriteJob(
	job diskWriteJob,
) error {
	seg := job.seg
	part := job.data

	switch seg.Kind {
	case RangeCandidate:
		/*
			If preserve=true, persist recording data.
			Otherwise it remains capture-only.
		*/
		if d.preserve {
			if errno := d.writeBacking(
				part,
				seg.Start,
				"CANDIDATE",
			); errno != 0 {
				return errno
			}
		}

		streamID := seg.StreamID

		if streamID == "" {
			streamID = "default"
		}

		d.feedCandidate(
			streamID,
			seg.StreamOffset,
			part,
		)

	case RangeMeta, RangeNormal, RangeUnknown:
		/*
			Filesystem state is persisted asynchronously.

			Crucially, the bytes are already visible to the STB through
			the RAM overlay, so the STB doesn't wait for this WriteAt().
		*/
		if err := d.writeBacking(
			part,
			seg.Start,
			"DATA",
		); err != 0 {
			return err
		}

		if err := d.processMetadataWrite(
			seg.Start,
			uint64(len(part)),
		); err != 0 {
			return err
		}
	}

	return nil
}

func (d *DiskNode) writeWorker() {
	for job := range d.writeQ {
		/*
			Flush/Fsync barrier.
		*/
		if job.barrier != nil {
			err := d.imgFile.Sync()

			job.barrier <- err
			close(job.barrier)

			continue
		}

		err := d.processWriteJob(job)

		if err != nil {
			d.workerMu.Lock()

			if d.workerErr == nil {
				d.workerErr = err
			}

			d.workerMu.Unlock()

			log.Printf(
				"async write worker failed: %v",
				err,
			)
		}

		d.removeCachedWrite(job.id)
	}
}

func (d *DiskNode) stageWrite(
	start uint64,
	data []byte,
) uint64 {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()

	id := d.cacheNextID
	d.cacheNextID++

	cp := append([]byte(nil), data...)

	d.cache = append(
		d.cache,
		cachedWrite{
			id:    id,
			start: start,
			data:  cp,
		},
	)

	return id
}

func (d *DiskNode) removeCachedWrite(id uint64) {
	d.cacheMu.Lock()
	defer d.cacheMu.Unlock()

	for i := range d.cache {
		if d.cache[i].id != id {
			continue
		}

		copy(
			d.cache[i:],
			d.cache[i+1:],
		)

		d.cache = d.cache[:len(d.cache)-1]

		return
	}
}

func (d *DiskNode) readBacking(
	dst []byte,
	off int64,
) (int, error) {
	n, err := d.imgFile.ReadAt(
		dst,
		off,
	)

	if err != nil && err != io.EOF {
		return 0, err
	}

	/*
		Overlay writes which have been acknowledged to the STB but
		have not reached the backing image yet.

		Later writes win over earlier writes.
	*/
	if len(dst) > 0 {
		reqStart := uint64(off)
		reqEnd := reqStart + uint64(len(dst))

		d.cacheMu.RLock()
		defer d.cacheMu.RUnlock()

		for _, w := range d.cache {
			wStart := w.start
			wEnd := w.start + uint64(len(w.data))

			if wEnd <= reqStart ||
				wStart >= reqEnd {
				continue
			}

			a := maxU64(reqStart, wStart)
			b := minU64(reqEnd, wEnd)

			srcStart := a - wStart
			dstStart := a - reqStart

			copy(
				dst[int(dstStart):int(dstStart+(b-a))],
				w.data[int(srcStart):int(srcStart+(b-a))],
			)
		}
	}

	/*
		ReadAt may return io.EOF with a partial read. The overlay above
		can still have supplied bytes that were not present in the
		backing image yet.
	*/
	if n < len(dst) {
		n = len(dst)
	}

	return n, nil
}
