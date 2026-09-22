// shm.go
package main

import (
	"os"
	"sync"
)

// ShmBuffer is a tmpfs-backed buffer.  It lives in /dev/shm, so it is not
// counted against the Go heap and is reclaimed by the kernel the moment the
// file descriptor is closed or the process exits.
type ShmBuffer struct {
	mu   sync.Mutex
	file *os.File
	path string
	size int64
}

// NewShmBuffer creates a uniquely-named file in /dev/shm.
func NewShmBuffer() (*ShmBuffer, error) {
	f, err := os.CreateTemp("/dev/shm", "tsdisk-buf-*")
	if err != nil {
		return nil, err
	}
	return &ShmBuffer{file: f, path: f.Name()}, nil
}

// Path returns the tmpfs path backing this buffer.
func (s *ShmBuffer) Path() string {
	return s.path
}

// Write appends data to the shm file and returns its previous offset.
func (s *ShmBuffer) Write(data []byte) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	off := s.size
	n, err := s.file.WriteAt(data, off)
	s.size += int64(n)
	return off, err
}

// ReadAt reads from the shm file.
func (s *ShmBuffer) ReadAt(dest []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.ReadAt(dest, off)
}

// Size returns the current length in bytes.
func (s *ShmBuffer) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

// Close closes and removes the tmpfs file.
func (s *ShmBuffer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.file.Close()
	_ = os.Remove(s.path)
	return err
}
