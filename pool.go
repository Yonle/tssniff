package main

import (
	"sync"
)

const (
	payloadPoolSize = 16 * 1024
	maxPooledSize   = 64 * 1024
)

var payloadPool = sync.Pool{
	New: func() any {
		return make([]byte, payloadPoolSize)
	},
}

func acquirePayload(size int) []byte {
	buf := payloadPool.Get().([]byte)

	if cap(buf) < size {
		payloadPool.Put(buf[:0])
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
