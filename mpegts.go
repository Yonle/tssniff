package main

import "log"

const (
	tsPacketSize = 188
	tsSyncByte   = 0x47

	/*
		Require 10 consecutive packets before declaring MPEG-TS.

		1880 bytes is tiny compared with the 16 KiB writes your STB
		usually generates.
	*/
	tsProbePackets = 10
	tsProbeSize    = tsPacketSize * tsProbePackets

	/*
		Keep enough bytes to bridge a packet/detection boundary across
		two contiguous writes.
	*/
	tsTailSize = tsProbeSize - 1

	/*
		Do not create enormous Hub messages.
	*/
	maxBroadcastPackets = 128
)

type TSChunk struct {
	Start uint64
	End   uint64

	Data []byte
}

type TSScanner struct {
	/*
		Unemitted bytes.

		This normally stays below one packet once detection succeeds,
		and below the detection window before detection succeeds.
	*/
	buffer []byte

	bufferStart uint64

	/*
		Physical end of the last contiguous write seen by this scanner.
	*/
	expected uint64

	haveExpected bool

	detected bool
}

func NewTSScanner() *TSScanner {
	return &TSScanner{
		buffer: make(
			[]byte,
			0,
			tsProbeSize,
		),
	}
}

func (s *TSScanner) reset(
	off uint64,
) {
	s.buffer = s.buffer[:0]

	s.bufferStart = off
	s.expected = off

	s.haveExpected = true

	s.detected = false
}

func (s *TSScanner) Feed(
	off uint64,
	data []byte,
) ([]TSChunk, bool) {
	if len(data) == 0 {
		return nil, s.detected
	}

	if s.detected {
		if off == s.expected {
			s.buffer = append(
				s.buffer,
				data...,
			)

			s.expected =
				off +
					uint64(len(data))

			return s.emit(), true
		}

		/*
			This write is physically non-contiguous.

			It may be unrelated metadata or another fragmented TS
			extent. Probe it independently.
		*/
		idx, ok := findMPEGTS(data)

		if !ok {
			return nil, false
		}

		s.buffer = append(
			s.buffer[:0],
			data[idx:]...,
		)

		s.bufferStart =
			off +
				uint64(idx)

		s.expected =
			off +
				uint64(len(data))

		if verbLog {
			log.Printf(
				"TS detector: MPEG-TS segment phys=%d",
				s.bufferStart,
			)
		}

		return s.emit(), true
	}
	/*
		Not detected yet:
		a non-contiguous write starts a new candidate.
	*/
	if !s.haveExpected || off != s.expected {
		s.reset(off)
	}

	s.buffer = append(
		s.buffer,
		data...,
	)

	s.expected =
		off +
			uint64(len(data))

	idx, ok := findMPEGTS(
		s.buffer,
	)

	if !ok {
		s.keepTail()
		return nil, false
	}

	if idx > 0 {
		copy(
			s.buffer,
			s.buffer[idx:],
		)

		s.buffer = s.buffer[:len(s.buffer)-idx]

		s.bufferStart += uint64(idx)
	}

	s.detected = true

	if verbLog {
		log.Printf(
			"TS detector: MPEG-TS detected phys=%d",
			s.bufferStart,
		)
	}

	return s.emit(), true
}

func (s *TSScanner) keepTail() {
	if len(s.buffer) <= tsTailSize {
		return
	}

	drop :=
		len(s.buffer) -
			tsTailSize

	copy(
		s.buffer,
		s.buffer[drop:],
	)

	s.buffer = s.buffer[:tsTailSize]

	s.bufferStart += uint64(drop)
}

func (s *TSScanner) emit() []TSChunk {
	if len(s.buffer) < tsPacketSize {
		return nil
	}

	var out []TSChunk

	for len(s.buffer) >= tsPacketSize {
		if !validTSPacket(
			s.buffer,
		) {
			idx, ok := findMPEGTS(
				s.buffer,
			)

			if !ok {
				s.keepTail()
				break
			}

			if idx > 0 {
				copy(
					s.buffer,
					s.buffer[idx:],
				)

				s.buffer = s.buffer[:len(s.buffer)-idx]

				s.bufferStart += uint64(idx)
			}

			if len(s.buffer) < tsPacketSize {
				break
			}
		}

		packetCount :=
			len(s.buffer) /
				tsPacketSize

		if packetCount >
			maxBroadcastPackets {
			packetCount = maxBroadcastPackets
		}

		validPackets := 0

		for i := 0; i < packetCount; i++ {
			off :=
				i *
					tsPacketSize

			if !validTSPacket(
				s.buffer[off:],
			) {
				break
			}

			validPackets++
		}

		if validPackets == 0 {
			/*
				Resynchronise one byte at a time.
			*/
			s.buffer = s.buffer[1:]

			s.bufferStart++

			continue
		}

		n :=
			validPackets *
				tsPacketSize

		payload := append(
			[]byte(nil),
			s.buffer[:n]...,
		)

		start := s.bufferStart

		end :=
			start +
				uint64(n)

		out = append(
			out,
			TSChunk{
				Start: start,
				End:   end,
				Data:  payload,
			},
		)

		copy(
			s.buffer,
			s.buffer[n:],
		)

		s.buffer = s.buffer[:len(s.buffer)-n]

		s.bufferStart = end

		if verbLog {
			log.Printf(
				"MPEG-TS chunk phys=[%d,%d) packets=%d",
				start,
				end,
				validPackets,
			)
		}
	}

	return out
}

func validTSPacket(
	data []byte,
) bool {
	if len(data) < tsPacketSize {
		return false
	}

	if data[0] != tsSyncByte {
		return false
	}

	/*
		Adaptation field control 00 is reserved.
	*/
	if data[3]&0x30 == 0 {
		return false
	}

	return true
}

func findMPEGTS(
	data []byte,
) (int, bool) {
	if len(data) < tsProbeSize {
		return 0, false
	}

	limit :=
		len(data) -
			tsProbeSize

	for i := 0; i <= limit; i++ {
		if data[i] != tsSyncByte {
			continue
		}

		ok := true

		for packet := 0; packet < tsProbePackets; packet++ {
			off :=
				i +
					packet*tsPacketSize

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
