package main

import (
	"log"
	"sync"
)

const (
	tsPacketSize = 188
	tsSyncByte   = 0x47

	// Number of consecutive MPEG-TS packets required before a candidate
	// is accepted as a transport stream.
	minTSPackets = 10

	// Bytes required to validate a complete MPEG-TS candidate.
	tsDetectionWindow = tsPacketSize * minTSPackets

	// Tail retained after a failed detection attempt.
	//
	// Keep one byte less than the full detection window so a future write
	// can complete a candidate that spans the write boundary.
	tsDetectionTail = tsDetectionWindow - 1

	// Maximum number of packets emitted in one Hub.Broadcast().
	maxBroadcastPackets = 128
)

type TSDetector struct {
	mu sync.Mutex

	streamID string

	haveOffset bool
	nextOffset uint64

	detected bool

	buffer []byte

	/*
		Backward writes cannot be appended to the current sequential
		stream because doing so would splice unrelated filesystem writes
		into the MPEG-TS byte stream.

		Instead, accumulate a separate probe.

		This is especially important for timeshift implementations which
		reuse an earlier physical/logical region after reaching the end
		of their current recording area.

		The probe is allowed to span multiple writes, including small
		512-byte writes.
	*/
	probeStreamID string

	probeHaveOffset bool
	probeStart      uint64
	probeNextOffset uint64

	probe []byte
}

func (d *TSDetector) resetProbeLocked() {
	d.probeStreamID = ""

	d.probeHaveOffset = false
	d.probeStart = 0
	d.probeNextOffset = 0

	d.probe = d.probe[:0]
}

func (d *TSDetector) resetLocked() {
	d.streamID = ""

	d.haveOffset = false
	d.nextOffset = 0

	d.detected = false

	d.buffer = d.buffer[:0]

	d.resetProbeLocked()
}

func (d *TSDetector) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.resetLocked()
}

/*
Feed receives candidate file-data bytes.

	streamID      identifies the logical recording/file.
	logicalOffset identifies where these bytes belong inside that stream.

For FAT32, logicalOffset is currently the physical disk offset.

For NTFS, logicalOffset is the file's logical byte offset, allowing
physical fragmentation without confusing it with a new stream.

IMPORTANT:

	A backwards offset does NOT mean the data is invalid.

	It only means the bytes cannot be appended to the current sequential
	MPEG-TS frontier.

	Backwards data is therefore independently probed for a strong MPEG-TS
	signature and may become a new sequential segment.
*/
func (d *TSDetector) Feed(streamID string, logicalOffset uint64, data []byte, emit func([]byte)) {
	if len(data) == 0 {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// A different stream starts a new detector session only when it
	// explicitly begins from offset 0.
	if d.streamID == "" {
		d.streamID = streamID
	} else if streamID != d.streamID {
		if logicalOffset == 0 {
			d.resetLocked()
			d.streamID = streamID
		} else {
			return
		}
	}

	// A backwards write may be the beginning of a new stream segment.
	// Probe it separately so the currently active detector is not destroyed
	// by filesystem metadata/bookkeeping writes.
	if d.haveOffset && logicalOffset < d.nextOffset {
		d.feedBackwardProbeLocked(streamID, logicalOffset, data, emit)
		return
	}

	// Physical/logical offset jumped forward.
	//
	// IMPORTANT:
	// If TS was already detected, do NOT clear d.detected.
	// FAT32 can legitimately write the same logical file through
	// non-contiguous physical sectors/clusters.
	if d.haveOffset && logicalOffset > d.nextOffset {
		if verbLog {
			log.Printf(
				"TS detector: forward gap stream=%q off=%d frontier=%d",
				streamID,
				logicalOffset,
				d.nextOffset,
			)
		}

		// Any incomplete packet tail belongs to the old region.
		d.buffer = d.buffer[:0]
		d.resetProbeLocked()
	}

	d.haveOffset = true
	d.nextOffset = logicalOffset + uint64(len(data))

	d.buffer = append(d.buffer, data...)
	d.processBufferLocked(streamID, emit)
}

/*
feedBackwardProbeLocked accumulates a backwards write separately from the
active sequential stream.

Returns true when the probe has become a new MPEG-TS segment and has been
promoted into the main detector.
*/
func (d *TSDetector) feedBackwardProbeLocked(
	streamID string,
	logicalOffset uint64,
	data []byte,
	emit func([]byte),
) bool {
	if len(data) == 0 {
		return false
	}

	/*
		Start a new probe unless this write continues the current probe
		contiguously.
	*/
	if !d.probeHaveOffset ||
		d.probeStreamID != streamID ||
		logicalOffset != d.probeNextOffset {

		d.resetProbeLocked()

		d.probeStreamID = streamID
		d.probeHaveOffset = true
		d.probeStart = logicalOffset
		d.probeNextOffset = logicalOffset
	}

	d.probe = append(
		d.probe,
		data...,
	)

	d.probeNextOffset =
		logicalOffset +
			uint64(len(data))

	/*
		IMPORTANT:

		Try detection BEFORE trimming.

		A complete detection window is 1880 bytes. If we trimmed to
		1879 bytes first, a sufficiently large backward write would
		never be able to pass findMPEGTSOffset().
	*/
	idx, ok := findMPEGTSOffset(
		d.probe,
	)

	if !ok {
		if verbLog {
			log.Printf(
				"TS detector: backward probe waiting stream=%q off=%d len=%d",
				streamID,
				d.probeStart,
				len(d.probe),
			)
		}

		if len(d.probe) > tsDetectionTail {
			drop := len(d.probe) - tsDetectionTail

			copy(
				d.probe,
				d.probe[drop:],
			)

			d.probe = d.probe[:tsDetectionTail]
			d.probeStart += uint64(drop)
		}

		return false
	}

	/*
		The backwards region really contains MPEG-TS.

		Promote it into the main sequential detector.

		This applies equally to FAT32 physical reuse and NTFS logical
		backtracking.
	*/
	if verbLog {
		log.Printf(
			"TS detector: backwards MPEG-TS segment stream=%q probe=%d..%d replacing frontier=%d",
			streamID,
			d.probeStart,
			d.probeNextOffset,
			d.nextOffset,
		)
	}

	candidate := make(
		[]byte,
		len(d.probe),
	)

	copy(
		candidate,
		d.probe,
	)

	candidateOffset := d.probeStart

	/*
		Reset the sequential state but keep the stream identity.
	*/
	d.buffer = d.buffer[:0]
	d.detected = false

	d.haveOffset = false
	d.nextOffset = 0

	d.resetProbeLocked()

	/*
		Discard bytes before the actual MPEG-TS sync point.
	*/
	if idx > 0 {
		candidate = candidate[idx:]
		candidateOffset += uint64(idx)
	}

	if len(candidate) == 0 {
		return false
	}

	d.haveOffset = true

	d.nextOffset =
		candidateOffset +
			uint64(len(candidate))

	d.buffer = append(
		d.buffer,
		candidate...,
	)

	d.streamID = streamID

	d.processBufferLocked(
		streamID,
		emit,
	)

	return true
}

func (d *TSDetector) processBufferLocked(
	streamID string,
	emit func([]byte),
) {
	/*
		Before MPEG-TS detection, look for the actual 188-byte sync
		pattern.
	*/
	if !d.detected {
		idx, ok := findMPEGTSOffset(
			d.buffer,
		)

		if !ok {
			/*
				Keep the maximum useful tail.

				The search itself happens BEFORE this trim, so a buffer
				which already contains a complete detection window is not
				accidentally reduced below the threshold.
			*/
			if len(d.buffer) > tsDetectionTail {
				drop := len(d.buffer) - tsDetectionTail

				copy(
					d.buffer,
					d.buffer[drop:],
				)

				d.buffer = d.buffer[:tsDetectionTail]
			}

			return
		}

		if idx > 0 {
			d.buffer = d.buffer[idx:]
		}

		d.detected = true

		if verbLog {
			log.Printf(
				"TS detector: MPEG-TS detected stream=%q",
				streamID,
			)
		}
	}

	/*
		Once detected, emit complete MPEG-TS packets only.
	*/
	for len(d.buffer) >= tsPacketSize {
		if !validTSPacket(
			d.buffer,
		) {
			idx, ok := findMPEGTSOffset(
				d.buffer,
			)

			if !ok {
				/*
					Keep a tail for possible resynchronisation on the
					next sequential write.

					Again, detection is attempted before trimming.
				*/
				if len(d.buffer) > tsDetectionTail {
					drop := len(d.buffer) - tsDetectionTail

					copy(
						d.buffer,
						d.buffer[drop:],
					)

					d.buffer = d.buffer[:tsDetectionTail]
				}

				return
			}

			if idx > 0 {
				d.buffer = d.buffer[idx:]
			}

			if len(d.buffer) < tsPacketSize {
				return
			}
		}

		packetCount :=
			len(d.buffer) /
				tsPacketSize

		if packetCount > maxBroadcastPackets {
			packetCount = maxBroadcastPackets
		}

		validPackets := 0

		for i := 0; i < packetCount; i++ {
			off := i * tsPacketSize

			if !validTSPacket(
				d.buffer[off:],
			) {
				break
			}

			validPackets++
		}

		if validPackets == 0 {
			d.buffer = d.buffer[1:]
			continue
		}

		n := validPackets * tsPacketSize

		payload := make(
			[]byte,
			n,
		)

		copy(
			payload,
			d.buffer[:n],
		)

		if verbLog {
			log.Printf(
				"MPEG-TS broadcast len=%d packets=%d",
				len(payload),
				validPackets,
			)
		}

		emit(payload)

		remaining :=
			len(d.buffer) - n

		if remaining > 0 {
			copy(
				d.buffer,
				d.buffer[n:],
			)
		}

		d.buffer = d.buffer[:remaining]
	}
}

/*
validTSPacket performs a minimal MPEG-TS header sanity check.

MPEG-TS:

	byte 0:
	    0x47 sync byte

	byte 3:
	    adaptation_field_control must not be 00.
*/
func validTSPacket(
	data []byte,
) bool {
	if len(data) < tsPacketSize {
		return false
	}

	if data[0] != tsSyncByte {
		return false
	}

	// adaptation_field_control == 00 is reserved.
	if data[3]&0x30 == 0 {
		return false
	}

	return true
}

/*
findMPEGTSOffset searches for a valid MPEG-TS sync position.

A valid candidate must contain minTSPackets consecutive packet positions
with a 0x47 sync byte and valid MPEG-TS headers.
*/
func findMPEGTSOffset(
	data []byte,
) (int, bool) {
	if len(data) < tsDetectionWindow {
		return 0, false
	}

	limit :=
		len(data) -
			tsDetectionWindow

	for i := 0; i <= limit; i++ {
		if data[i] != tsSyncByte {
			continue
		}

		ok := true

		for j := 0; j < minTSPackets; j++ {
			off :=
				i +
					j*tsPacketSize

			if !validTSPacket(
				data[off:],
			) {
				ok = false
				break
			}
		}

		if ok {
			return i, true
		}
	}

	return 0, false
}

func scanTSPayload(
	data []byte,
) (int, bool) {
	return findMPEGTSOffset(
		data,
	)
}
