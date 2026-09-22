package main

import (
	"sync"
)

const (
	payloadPoolSize = 128 * 1024
	maxPooledSize   = 256 * 1024
)

var payloadPool = sync.Pool{
	New: func() any {
		return make([]byte, payloadPoolSize)
	},
}

func acquirePayload(size int) []byte {
	buf := payloadPool.Get().([]byte)

	if cap(buf) < size {
		return make([]byte, size)
	}

	return buf[:size]
}

func releasePayload(buf []byte) {
	if cap(buf) > maxPooledSize {
		return
	}

	payloadPool.Put(buf[:0])
}
