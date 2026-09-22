package main

import (
	"errors"
	"io"
	"log"
	"os"
	"sync/atomic"
)

type PendingWrite struct {
	Offset      uint64
	SpoolOffset int64
	Length      int
}

type overlayState struct {
	writes []PendingWrite
}

type Overlay struct {
	fd         *os.File
	quarantine *os.File
	tracker    Tracker
	hub        *Hub

	cmd     chan overlayCommand
	pending atomic.Pointer[overlayState]
	readers atomic.Int64
}

type overlayOp uint8

const (
	overlayQueue overlayOp = iota
	overlayReserveTS
	overlayRefresh
	overlayClose
)

type overlayCommand struct {
	op          overlayOp
	offset      uint64
	name        string
	data        []byte
	done        chan overlayResult
	closeSignal chan struct{}
}

type overlayResult struct {
	start uint64
	data  []byte
	err   error
}

func NewOverlay(
	fd *os.File,
	quarantine *os.File,
	tracker Tracker,
	hub *Hub,
) *Overlay {
	o := &Overlay{
		fd:         fd,
		quarantine: quarantine,
		tracker:    tracker,
		hub:        hub,
		cmd:        make(chan overlayCommand, 128),
	}
	o.pending.Store(&overlayState{})
	go o.run()
	return o
}

func (o *Overlay) Pending() []PendingWrite {
	return o.pending.Load().writes
}

func (o *Overlay) BeginRead() []PendingWrite {
	o.readers.Add(1)
	return o.Pending()
}

func (o *Overlay) EndRead() {
	o.readers.Add(-1)
}

func (o *Overlay) Queue(offset uint64, data []byte) error {
	if len(data) == 0 {
		return nil
	}

	done := make(chan overlayResult, 1)
	o.cmd <- overlayCommand{
		op:     overlayQueue,
		offset: offset,
		data:   data,
		done:   done,
	}

	return (<-done).err
}

func (o *Overlay) ReserveTSRange(
	name string,
	start uint64,
	data []byte,
) (uint64, []byte) {
	if len(data) == 0 || name == "" {
		return start, nil
	}

	done := make(chan overlayResult, 1)
	o.cmd <- overlayCommand{
		op:     overlayReserveTS,
		offset: start,
		name:   name,
		data:   data,
		done:   done,
	}

	result := <-done
	return result.start, result.data
}

func (o *Overlay) RefreshAndResolve() error {
	done := make(chan overlayResult, 1)
	o.cmd <- overlayCommand{
		op:   overlayRefresh,
		done: done,
	}

	return (<-done).err
}

func (o *Overlay) Close() {
	done := make(chan struct{})
	o.cmd <- overlayCommand{
		op:          overlayClose,
		closeSignal: done,
	}
	<-done
}

func (o *Overlay) run() {
	pending := o.pending.Load().writes
	nextSpool := int64(0)
	hwm := make(map[string]uint64)

	for cmd := range o.cmd {
		switch cmd.op {
		case overlayQueue:
			o.compactIfIdle(&pending, &nextSpool)

			err := o.queue(&pending, &nextSpool, cmd.offset, cmd.data)
			cmd.done <- overlayResult{err: err}

		case overlayReserveTS:
			o.compactIfIdle(&pending, &nextSpool)

			start, data := reserveTS(hwm, cmd.name, cmd.offset, cmd.data)
			cmd.done <- overlayResult{
				start: start,
				data:  data,
			}

		case overlayRefresh:
			err := o.tracker.Refresh()
			if err == nil {
				err = o.resolve(&pending, &nextSpool, hwm)
			}
			cmd.done <- overlayResult{err: err}

		case overlayClose:
			if cmd.closeSignal != nil {
				close(cmd.closeSignal)
			}
			return
		}
	}
}

func (o *Overlay) queue(
	pending *[]PendingWrite,
	nextSpool *int64,
	offset uint64,
	data []byte,
) error {
	if o.quarantine == nil {
		return errors.New("quarantine file is not open")
	}

	spoolOffset := *nextSpool
	n, err := o.quarantine.WriteAt(data, spoolOffset)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}

	next := make([]PendingWrite, len(*pending)+1)
	copy(next, *pending)
	next[len(*pending)] = PendingWrite{
		Offset:      offset,
		SpoolOffset: spoolOffset,
		Length:      n,
	}
	*pending = next
	o.publish(*pending)

	if verbLog {
		log.Printf(
			"QUARANTINE offset=%d size=%d",
			offset,
			n,
		)
	}

	*nextSpool += int64(n)
	return nil
}

func reserveTS(
	hwm map[string]uint64,
	name string,
	start uint64,
	data []byte,
) (uint64, []byte) {
	mark := hwm[name]

	if start < mark {
		// If the write jumps backward heavily, the file was likely overwritten/truncated.
		if mark-start > 1024*1024 {
			mark = 0
		} else {
			skip := mark - start
			if skip >= uint64(len(data)) {
				return 0, nil
			}

			start += skip
			data = data[skip:]
		}
	}

	hwm[name] = start + uint64(len(data))
	return start, data
}

func (o *Overlay) resolve(
	pending *[]PendingWrite,
	nextSpool *int64,
	hwm map[string]uint64,
) error {
	if len(*pending) == 0 {
		o.compactIfIdle(pending, nextSpool)
		return nil
	}

	stillPending := make([]PendingWrite, 0, len(*pending))
	buf := make([]byte, 0, 128*1024)
	var tsBatch [][]byte

	for _, p := range *pending {
		if cap(buf) < p.Length {
			buf = make([]byte, p.Length)
		} else {
			buf = buf[:p.Length]
		}

		n, err := o.quarantine.ReadAt(buf, p.SpoolOffset)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n != p.Length {
			return io.ErrUnexpectedEOF
		}

		segments := o.tracker.Classify(
			p.Offset,
			uint64(p.Length),
		)

		tsBatch = tsBatch[:0]

		for _, seg := range segments {
			rel := seg.Start - p.Offset
			size := seg.End - seg.Start
			part := buf[int(rel):int(rel+size)]

			switch seg.Kind {
			case RangeTS:
				start, data := reserveTS(
					hwm,
					seg.Name,
					seg.Start,
					part,
				)
				if len(data) == 0 {
					continue
				}

				if verbLog {
					log.Printf(
						"REPLAY TS offset=%d size=%d file=%s",
						start,
						len(data),
						seg.Name,
					)
				}

				tsBatch = append(tsBatch, data)

			case RangeNormal, RangeMeta:
				nn, err := o.fd.WriteAt(part, int64(seg.Start))
				if err != nil {
					return err
				}
				if nn != len(part) {
					return io.ErrShortWrite
				}

			case RangeUnknown:
				stillPending = append(
					stillPending,
					PendingWrite{
						Offset:      seg.Start,
						SpoolOffset: p.SpoolOffset + int64(rel),
						Length:      int(size),
					},
				)
			}
		}

		if len(tsBatch) != 0 {
			o.hub.BroadcastBatch(tsBatch)
		}
	}

	*pending = stillPending
	o.publish(*pending)

	if len(*pending) == 0 && o.readers.Load() == 0 {
		if err := o.quarantine.Truncate(0); err != nil {
			return err
		}
		*nextSpool = 0
	}

	return nil
}

func (o *Overlay) publish(pending []PendingWrite) {
	if len(pending) == 0 {
		o.pending.Store(&overlayState{})
		return
	}

	o.pending.Store(&overlayState{writes: pending})
}

func (o *Overlay) compactIfIdle(
	pending *[]PendingWrite,
	nextSpool *int64,
) {
	if len(*pending) != 0 ||
		*nextSpool == 0 ||
		o.readers.Load() != 0 {
		return
	}

	if err := o.quarantine.Truncate(0); err != nil {
		log.Printf("quarantine truncate: %v", err)
		return
	}

	*nextSpool = 0
}
