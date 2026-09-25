package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"sync"
	"syscall"
)

var (
	ErrSHMClosed        = errors.New("shm overlay closed")
	ErrSHMStale         = errors.New("shm generation is stale")
	ErrSHMInvalidRange  = errors.New("invalid shm range")
	ErrSHMOutsideVolume = errors.New("shm range outside volume")
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
		Sorted, non-overlapping logical overlay extents.

		The bytes themselves live in /dev/shm.
		This slice contains only metadata:

		    physical/logical range
		    generation number
	*/
	extents []ShmExtent
}

func NewSHMDisk(
	base io.ReaderAt,
	size uint64,
) (*SHMDisk, error) {
	if base == nil {
		return nil, fmt.Errorf("nil SHM base reader")
	}

	if size == 0 {
		return nil, fmt.Errorf("zero SHM size")
	}

	file, err := os.CreateTemp(
		"/dev/shm",
		"tssniff-*.img",
	)
	if err != nil {
		return nil, fmt.Errorf(
			"create /dev/shm overlay: %w",
			err,
		)
	}

	path := file.Name()

	if err := file.Truncate(int64(size)); err != nil {
		file.Close()

		if path != "" {
			_ = os.Remove(path)
		}

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

/*
StageWrite writes a new logical version into the /dev/shm overlay.

The returned generation identifies exactly this version of the range.

The physical sparse image is NOT touched.
*/
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
		return ShmExtent{}, ErrSHMOutsideVolume
	}

	end := off + uint64(len(data))

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ShmExtent{}, ErrSHMClosed
	}

	n, err := s.file.WriteAt(
		data,
		int64(off),
	)
	if err != nil {
		return ShmExtent{}, err
	}

	if n != len(data) {
		return ShmExtent{}, io.ErrShortWrite
	}

	gen := s.nextGen
	s.nextGen++

	extent := ShmExtent{
		Start: off,
		End:   end,
		Gen:   gen,
	}

	s.replaceRangeLocked(extent)

	return extent, nil
}

func (s *SHMDisk) ReadAt(
	dst []byte,
	off int64,
) (int, error) {
	if off < 0 {
		return 0, ErrSHMInvalidRange
	}

	if len(dst) == 0 {
		return 0, nil
	}

	start := uint64(off)

	if start >= s.size ||
		uint64(len(dst)) > s.size-start {
		return 0, ErrSHMOutsideVolume
	}

	/*
		Read the physical base first.

		The SHM overlay is then applied on top.

		This preserves the sparse-overlay model:
		unchanged areas do not consume RAM in /dev/shm.
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

	reqEnd := start + uint64(len(dst))

	/*
		Take a snapshot of the currently visible overlay ranges.

		The actual SHM reads happen while holding RLock so a release/punch
		cannot modify the overlay bytes halfway through the copy.
	*/
	s.mu.RLock()

	if !s.closed {
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

			src := a
			dstOff := a - start

			if _, err := s.file.ReadAt(
				dst[int(dstOff):int(dstOff+(b-a))],
				int64(src),
			); err != nil {
				s.mu.RUnlock()

				return 0, err
			}
		}
	}

	s.mu.RUnlock()

	/*
		ReadAt may have returned EOF for the physical base while the SHM
		overlay completely satisfied the request.

		The logical disk is still considered fully readable.
	*/
	return len(dst), nil
}

/*
Snapshot returns the overlay generations currently covering a range.

This is used when constructing a physical operation so the writer knows
which logical version the operation belongs to.
*/
func (s *SHMDisk) Snapshot(
	start,
	end uint64,
) []ShmExtent {
	if end <= start {
		return nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil
	}

	out := make(
		[]ShmExtent,
		0,
		4,
	)

	for _, e := range s.extents {
		if e.End <= start {
			continue
		}

		if e.Start >= end {
			break
		}

		a := maxU64(
			start,
			e.Start,
		)

		b := minU64(
			end,
			e.End,
		)

		if a >= b {
			continue
		}

		out = append(
			out,
			ShmExtent{
				Start: a,
				End:   b,
				Gen:   e.Gen,
			},
		)
	}

	return out
}

/*
CurrentMatching intersects planned generations with the currently live
overlay generations.

If another logical write replaced part of the range, the newer generation
will not match and that part is omitted.

This is what prevents an old physical writer operation from accidentally
persisting a newer capture-only write.
*/
func (s *SHMDisk) CurrentMatching(
	planned []ShmExtent,
) []ShmExtent {
	if len(planned) == 0 {
		return nil
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil
	}

	out := make(
		[]ShmExtent,
		0,
		len(planned),
	)

	for _, p := range planned {
		if p.End <= p.Start {
			continue
		}

		for _, e := range s.extents {
			if e.End <= p.Start {
				continue
			}

			if e.Start >= p.End {
				break
			}

			if e.Gen != p.Gen {
				continue
			}

			a := maxU64(
				p.Start,
				e.Start,
			)

			b := minU64(
				p.End,
				e.End,
			)

			if a >= b {
				continue
			}

			out = append(
				out,
				ShmExtent{
					Start: a,
					End:   b,
					Gen:   p.Gen,
				},
			)
		}
	}

	return coalesceShmExtents(out)
}

/*
ReadGeneration reads a range only if that exact generation still owns the
entire requested range.

The writer uses this after CurrentMatching().

If another write replaced the range in the meantime, ErrSHMStale is returned
and the newer writer operation gets to handle it.
*/
func (s *SHMDisk) ReadGeneration(
	dst []byte,
	extent ShmExtent,
) error {
	if len(dst) == 0 {
		return nil
	}

	if uint64(len(dst)) != extent.End-extent.Start {
		return ErrSHMInvalidRange
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return ErrSHMClosed
	}

	if !s.coversGenerationLocked(extent) {
		return ErrSHMStale
	}

	n, err := s.file.ReadAt(
		dst,
		int64(extent.Start),
	)

	if err != nil {
		if err == io.EOF && n == len(dst) {
			return nil
		}
		return err
	}

	if n != len(dst) {
		return io.ErrUnexpectedEOF
	}

	return nil
}

/*
ReleaseIfCurrent removes SHM overlay data only when the generation still
matches.

For normal persisted writes this frees /dev/shm after WriteAt() succeeds.

For replay/hole-punch cleanup it makes sure a newer logical write cannot
accidentally disappear.
*/
func (s *SHMDisk) ReleaseIfCurrent(
	planned []ShmExtent,
) error {
	if len(planned) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrSHMClosed
	}

	var firstErr error

	for _, p := range planned {
		if p.End <= p.Start {
			continue
		}

		matches := s.matchGenerationLocked(p)

		for _, m := range matches {
			if err := s.punchLocked(
				m.Start,
				m.End-m.Start,
			); err != nil {
				if firstErr == nil {
					firstErr = err
				}

				continue
			}

			s.removeRangeGenerationLocked(
				m.Start,
				m.End,
				m.Gen,
			)
		}
	}

	return firstErr
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

			Keep the old generation on either side of the new write.
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
			out = append(
				out,
				newExtent,
			)

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
		out = append(
			out,
			newExtent,
		)
	}

	s.extents = coalesceShmExtents(out)
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

func (s *SHMDisk) matchGenerationLocked(
	want ShmExtent,
) []ShmExtent {
	out := make(
		[]ShmExtent,
		0,
		2,
	)

	for _, e := range s.extents {
		if e.End <= want.Start {
			continue
		}

		if e.Start >= want.End {
			break
		}

		if e.Gen != want.Gen {
			continue
		}

		a := maxU64(
			want.Start,
			e.Start,
		)

		b := minU64(
			want.End,
			e.End,
		)

		if a < b {
			out = append(
				out,
				ShmExtent{
					Start: a,
					End:   b,
					Gen:   e.Gen,
				},
			)
		}
	}

	return out
}

func (s *SHMDisk) removeRangeGenerationLocked(
	start,
	end,
	gen uint64,
) {
	if end <= start {
		return
	}

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

	s.extents = coalesceShmExtents(out)
}

func (s *SHMDisk) punchLocked(
	off,
	length uint64,
) error {
	if length == 0 {
		return nil
	}

	err := syscall.Fallocate(
		int(s.file.Fd()),
		shmFallocPunchHole|shmFallocKeepSize,
		int64(off),
		int64(length),
	)

	if err != nil {
		log.Printf(
			"SHM punch hole failed off=%d len=%d: %v",
			off,
			length,
			err,
		)

		return err
	}

	return nil
}

func coalesceShmExtents(
	in []ShmExtent,
) []ShmExtent {
	if len(in) == 0 {
		return nil
	}

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

		out = append(
			out,
			e,
		)
	}

	return out
}

func sortShmExtents(
	in []ShmExtent,
) {
	sort.Slice(
		in,
		func(i, j int) bool {
			if in[i].Start != in[j].Start {
				return in[i].Start < in[j].Start
			}

			if in[i].End != in[j].End {
				return in[i].End < in[j].End
			}

			return in[i].Gen < in[j].Gen
		},
	)
}
