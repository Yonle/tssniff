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
func (d *TSDetector) Feed(
	streamID string,
	logicalOffset uint64,
	data []byte,
	emit func([]byte),
) {
	if len(data) == 0 {
		return
	}

	if streamID == "" {
		streamID = "default"
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	/*
		A different stream means a different recording/file.
	*/
	if d.streamID != streamID {
		if d.streamID == "" {
			d.streamID = streamID

			if verbLog {
				log.Printf(
					"TS detector: new stream=%q",
					streamID,
				)
			}
		} else if logicalOffset == 0 {
			if verbLog {
				log.Printf(
					"TS detector: new recording stream=%q replacing=%q",
					streamID,
					d.streamID,
				)
			}

			d.resetLocked()
			d.streamID = streamID
		} else {
			if verbLog {
				log.Printf(
					"TS detector: ignore other stream=%q active=%q logical=%d",
					streamID,
					d.streamID,
					logicalOffset,
				)
			}

			return
		}
	}

	/*
		Determine whether this write belongs to the current sequential
		frontier.
	*/
	if d.haveOffset {
		/*
			Backward write.

			Do NOT append it to d.buffer.

			Instead, accumulate a separate probe until either:

			    - it contains enough valid MPEG-TS packets
			    - it becomes discontinuous
			    - a forward/current write resumes the existing stream
		*/
		if logicalOffset < d.nextOffset {
			if d.feedBackwardProbeLocked(
				streamID,
				logicalOffset,
				data,
				emit,
			) {
				/*
					The backward probe became a new sequential
					segment and was promoted into d.buffer.

					That function already consumed the bytes.
				*/
				return
			}

			/*
				Still only a probe. Do not disturb the current
				MPEG-TS segment.
			*/
			if verbLog {
				log.Printf(
					"TS detector: defer backwards write stream=%q off=%d frontier=%d",
					streamID,
					logicalOffset,
					d.nextOffset,
				)
			}

			return
		}

		/*
			A forward discontinuity means we don't have the bytes between
			the current frontier and this write.

			Do not stitch them together into one MPEG-TS stream.
		*/
		if logicalOffset > d.nextOffset {
			if verbLog {
				log.Printf(
					"TS detector: forward gap stream=%q off=%d frontier=%d, reset",
					streamID,
					logicalOffset,
					d.nextOffset,
				)
			}

			d.buffer = d.buffer[:0]
			d.detected = false

			/*
				A forward continuation makes an older backward probe
				unrelated to the new frontier.
			*/
			d.resetProbeLocked()
		} else {
			/*
				Exactly contiguous with the current stream.

				Any incomplete backward probe is no longer a candidate
				for this sequential segment.
			*/
			d.resetProbeLocked()
		}
	} else {
		/*
			There is no established frontier yet.

			A previous backward probe is irrelevant if this write does
			not continue it.
		*/
		if d.probeHaveOffset {
			if logicalOffset != d.probeNextOffset ||
				streamID != d.probeStreamID {
				d.resetProbeLocked()
			}
		}
	}

	/*
		Current write belongs to the active sequential stream.
	*/
	d.haveOffset = true
	d.nextOffset = logicalOffset + uint64(len(data))

	d.buffer = append(
		d.buffer,
		data...,
	)

	d.processBufferLocked(
		streamID,
		emit,
	)
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
		Do not allow a pathological non-TS backwards region to consume
		unbounded memory.

		We only need enough tail data to detect a future 10-packet sync
		pattern crossing a write boundary.
	*/
	const keep =
		tsPacketSize*minTSPackets - 1

	if len(d.probe) > keep {
		drop := len(d.probe) - keep

		copy(
			d.probe,
			d.probe[drop:],
		)

		d.probe = d.probe[:keep]

		/*
			The exact logical start of the retained probe moved forward.
		*/
		d.probeStart += uint64(drop)
	}

	/*
		Require the same strong MPEG-TS signature used by normal
		detection.
	*/
	idx, ok := findMPEGTSOffset(
		d.probe,
	)

	if !ok {
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
				Keep enough tail bytes so a sync pattern may span
				two separate filesystem writes.
			*/
			const keep =
				tsPacketSize*minTSPackets - 1

			if len(d.buffer) > keep {
				drop := len(d.buffer) - keep

				copy(
					d.buffer,
					d.buffer[drop:],
				)

				d.buffer = d.buffer[:keep]
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
				*/
				const keep =
					tsPacketSize*minTSPackets - 1

				if len(d.buffer) > keep {
					drop := len(d.buffer) - keep

					copy(
						d.buffer,
						d.buffer[drop:],
					)

					d.buffer = d.buffer[:keep]
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
func validTSPacket(data []byte) bool {
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
func findMPEGTSOffset(data []byte) (int, bool) {
	need := tsPacketSize * minTSPackets

	if len(data) < need {
		return 0, false
	}

	limit := len(data) - need

	for i := 0; i <= limit; i++ {
		if data[i] != tsSyncByte {
			continue
		}

		ok := true

		for j := 0; j < minTSPackets; j++ {
			off := i + j*tsPacketSize

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