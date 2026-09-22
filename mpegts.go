package main

const (
	tsPacketSize = 188
	tsSyncByte   = 0x47

	// maxBroadcastPackets bounds how many TS packets end up in a single
	// hub.Broadcast. Smaller chunks keep per-client channel entries small so
	// a slow client cannot balloon RSS. 128 * 188 ≈ 24 KB per broadcast.
	maxBroadcastPackets = 128
)

// findMPEGTSOffset validates TS alignment using 188-byte stride verification
func findMPEGTSOffset(data []byte) (int, bool) {
	if len(data) < tsPacketSize {
		return 0, false
	}

	for i := 0; i <= len(data)-tsPacketSize; i++ {
		if data[i] != tsSyncByte {
			continue
		}

		// Verify 188-byte stride across available buffer to prevent false positives on payload bytes
		validStride := true
		checked := 0
		for j := i; j+tsPacketSize <= len(data) && checked < 3; j += tsPacketSize {
			if data[j] != tsSyncByte {
				validStride = false
				break
			}
			checked++
		}

		if validStride {
			return i, true
		}
	}

	return 0, false
}

func scanTSPayload(data []byte) (int, bool) {
	return findMPEGTSOffset(data)
}
