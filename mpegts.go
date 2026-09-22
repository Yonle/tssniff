package main

const (
	tsPacketSize = 188
	tsSyncByte   = 0x47

	// Minimum number of consecutive, 188-byte-aligned 0x47 bytes required
	// before we accept a region as transport stream data.  A 512-byte
	// metadata update can hold at most 2 packets, so anything >= 10
	// rejects false positives.
	minTSPackets = 10

	// maxBroadcastPackets bounds the size of a single hub.Broadcast.
	maxBroadcastPackets = 128
)

func findMPEGTSOffset(data []byte) (int, bool) {
	if len(data) < tsPacketSize*minTSPackets {
		return 0, false
	}
	limit := len(data) - tsPacketSize*minTSPackets
	for i := 0; i <= limit; i++ {
		if data[i] != tsSyncByte {
			continue
		}
		ok := true
		for j := i; j < i+tsPacketSize*minTSPackets; j += tsPacketSize {
			if data[j] != tsSyncByte {
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

func scanTSPayload(data []byte) (int, bool) {
	return findMPEGTSOffset(data)
}
