package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"syscall"
)

var (
	ErrSHMClosed        = errors.New("shm overlay closed")
	ErrSHMInvalidRange  = errors.New("invalid shm range")
	ErrSHMOutsideVolume = errors.New("shm range outside volume")
	ErrSHMStale         = errors.New("shm generation is stale")
)

const (
	shmFallocKeepSize  uint32 = 0x01
	shmFallocPunchHole uint32 = 0x02
)

type ShmExtent struct {
	Start uint64
	End   uint64
	Gen   uint64
}

type SHMDisk struct {
	file *os.File
	base io.ReaderAt
	size uint64

	path string

	mu      sync.RWMutex
	nextGen uint64
	closed  bool

	/*
		Sorted, non-overlapping overlay extents.
	*/
	extents []ShmExtent
}

func NewSHMDisk(
	base io.ReaderAt,
	size uint64,
) (*SHMDisk, error) {
	if base == nil {
		return nil, fmt.Errorf(
			"nil SHM base reader",
		)
	}

	if size == 0 {
		return nil, fmt.Errorf(
			"zero SHM size",
		)
	}

	file, err := os.CreateTemp(
		"/dev/shm",
		"tssniff-*.img",
	)
	if err != nil {
		return nil, fmt.Errorf(
			"create SHM overlay: %w",
			err,
		)
	}

	path := file.Name()

	if err := file.Truncate(
		int64(size),
	); err != nil {

		_ = file.Close()
		_ = os.Remove(path)

		return nil, fmt.Errorf(
			"resize SHM overlay: %w",
			err,
		)
	}

	return &SHMDisk{
		file:    file,
		base:    base,
		size:    size,
		path:    path,
		nextGen: 1,
	}, nil
}

func (s *SHMDisk) Size() uint64 {
	return s.size
}

func (s *SHMDisk) StageWrite(
	data []byte,
	off uint64,
) (ShmExtent, error) {
	if len(data) == 0 {
		return ShmExtent{
			Start: off,
			End:   off,
		}, nil
	}

	if off >= s.size ||
		uint64(len(data)) > s.size-off {
		return ShmExtent{},
			ErrSHMOutsideVolume
	}

	end :=
		off +
			uint64(len(data))

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ShmExtent{},
			ErrSHMClosed
	}

	n, err := s.file.WriteAt(
		data,
		int64(off),
	)
	if err != nil {
		return ShmExtent{}, err
	}

	if n != len(data) {
		return ShmExtent{},
			io.ErrShortWrite
	}

	extent := ShmExtent{
		Start: off,
		End:   end,
		Gen:   s.nextGen,
	}

	s.nextGen++

	s.replaceRangeLocked(extent)

	return extent, nil
}

func (s *SHMDisk) ReadAt(
	dst []byte,
	off int64,
) (int, error) {
	if off < 0 {
		return 0,
			ErrSHMInvalidRange
	}

	if len(dst) == 0 {
		return 0, nil
	}

	start := uint64(off)

	if start >= s.size ||
		uint64(len(dst)) > s.size-start {
		return 0,
			ErrSHMOutsideVolume
	}

	/*
		Start from the physical image.
	*/
	n, baseErr := s.base.ReadAt(
		dst,
		off,
	)

	if n < len(dst) {
		clear(dst[n:])
	}

	if baseErr != nil &&
		baseErr != io.EOF {
		return 0, baseErr
	}

	reqEnd :=
		start +
			uint64(len(dst))

	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return len(dst), nil
	}

	for _, e := range s.extents {
		if e.End <= start {
			continue
		}

		if e.Start >= reqEnd {
			break
		}

		a := maxU64(
			start,
			e.Start,
		)

		b := minU64(
			reqEnd,
			e.End,
		)

		if a >= b {
			continue
		}

		dstOff :=
			a - start

		nread,
			err := s.file.ReadAt(
			dst[int(dstOff):int(dstOff+(b-a))],
			int64(a),
		)

		if err != nil &&
			err != io.EOF {
			return 0, err
		}

		if nread != int(b-a) {
			return 0,
				io.ErrUnexpectedEOF
		}
	}

	return len(dst), nil
}

func (s *SHMDisk) ReleaseIfCurrent(
	planned []ShmExtent,
) error {
	if len(planned) == 0 {
		return nil
	}

	var firstErr error

	for _, p := range planned {
		if p.End <= p.Start {
			continue
		}

		s.mu.Lock()

		if s.closed {
			s.mu.Unlock()

			if firstErr == nil {
				firstErr = ErrSHMClosed
			}

			continue
		}

		/*
			The exact generation must still own this range.

			If a newer write replaced it, leave the newer overlay
			untouched.
		*/
		if !s.coversGenerationLocked(p) {
			s.mu.Unlock()
			continue
		}

		err := s.punchLocked(
			p.Start,
			p.End-p.Start,
		)

		if err == nil {
			s.removeRangeGenerationLocked(
				p.Start,
				p.End,
				p.Gen,
			)
		}

		s.mu.Unlock()

		if err != nil &&
			firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

func (s *SHMDisk) coversGenerationLocked(
	want ShmExtent,
) bool {
	cursor := want.Start

	for _, e := range s.extents {
		if e.End <= cursor {
			continue
		}

		if e.Start > cursor {
			return false
		}

		if e.Gen != want.Gen {
			return false
		}

		if e.End >= want.End {
			return true
		}

		cursor = e.End
	}

	return false
}

func (s *SHMDisk) replaceRangeLocked(
	newExtent ShmExtent,
) {
	out := make(
		[]ShmExtent,
		0,
		len(s.extents)+1,
	)

	inserted := false

	for _, e := range s.extents {
		if e.End <= newExtent.Start {
			out = append(out, e)

			continue
		}

		if e.Start >= newExtent.End {
			if !inserted {
				out = append(out, newExtent)

				inserted = true
			}

			out = append(out, e)

			continue
		}

		/*
			Overlap.
		*/
		if e.Start < newExtent.Start {
			out = append(
				out,
				ShmExtent{
					Start: e.Start,
					End:   newExtent.Start,
					Gen:   e.Gen,
				},
			)
		}

		if !inserted {
			out = append(out, newExtent)

			inserted = true
		}

		if e.End > newExtent.End {
			out = append(
				out,
				ShmExtent{
					Start: newExtent.End,
					End:   e.End,
					Gen:   e.Gen,
				},
			)
		}
	}

	if !inserted {
		out = append(out, newExtent)
	}

	s.extents = coalesceSHM(out)
}

func (s *SHMDisk) removeRangeGenerationLocked(
	start,
	end,
	gen uint64,
) {
	out := make(
		[]ShmExtent,
		0,
		len(s.extents),
	)

	for _, e := range s.extents {
		if e.Gen != gen ||
			e.End <= start ||
			e.Start >= end {
			out = append(out, e)

			continue
		}

		if e.Start < start {
			out = append(
				out,
				ShmExtent{
					Start: e.Start,
					End:   start,
					Gen:   e.Gen,
				},
			)
		}

		if e.End > end {
			out = append(
				out,
				ShmExtent{
					Start: end,
					End:   e.End,
					Gen:   e.Gen,
				},
			)
		}
	}

	s.extents = coalesceSHM(out)
}

func (s *SHMDisk) punchLocked(
	off,
	length uint64,
) error {
	if length == 0 {
		return nil
	}

	return syscall.Fallocate(
		int(s.file.Fd()),
		shmFallocPunchHole|shmFallocKeepSize,
		int64(off),
		int64(length),
	)
}

func (s *SHMDisk) Close() error {
	s.mu.Lock()

	if s.closed {
		s.mu.Unlock()
		return nil
	}

	s.closed = true

	file := s.file
	path := s.path

	s.file = nil
	s.extents = nil

	s.mu.Unlock()

	var firstErr error

	if file != nil {
		if err := file.Close(); err != nil {
			firstErr = err
		}
	}

	if path != "" {
		if err := os.Remove(path); err != nil &&
			!errors.Is(err, os.ErrNotExist) {

			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

func coalesceSHM(
	in []ShmExtent,
) []ShmExtent {
	if len(in) == 0 {
		return nil
	}

	sort.Slice(
		in,
		func(i, j int) bool {
			if in[i].Start != in[j].Start {
				return in[i].Start < in[j].Start
			}

			return in[i].End < in[j].End
		},
	)

	out := make(
		[]ShmExtent,
		0,
		len(in),
	)

	for _, e := range in {
		if e.End <= e.Start {
			continue
		}

		if len(out) > 0 {
			last := &out[len(out)-1]

			if last.End == e.Start &&
				last.Gen == e.Gen {

				last.End = e.End
				continue
			}
		}

		out = append(out, e)
	}

	return out
}

func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}

	return b
}

func minU64(a, b uint64) uint64 {
	if a < b {
		return a
	}

	return b
}
