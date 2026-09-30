package main

import (
	"log"
	"os"
	"sync"
	"syscall"
)

const (
	writerQueueDepth = 2048

	writerHistoryCapacity = 131072

	fallocKeepSize  uint32 = 0x01
	fallocPunchHole uint32 = 0x02
)

type writerCommandKind uint8

const (
	writerWrite writerCommandKind = iota
	writerPunch
	writerBarrier
)

type writerCommand struct {
	kind writerCommandKind

	write   WriteEvent
	punch   PunchRequest
	barrier chan error
}

type WriteRecord struct {
	Seq   uint64
	Start uint64
	End   uint64
}

type Writer struct {
	disk *os.File
	shm  *SHMDisk

	in chan writerCommand

	wg sync.WaitGroup

	/*
		Only the writer goroutine touches this.
	*/
	recent [writerHistoryCapacity]WriteRecord

	recentHead  int
	recentCount int
}

func NewWriter(
	disk *os.File,
	shm *SHMDisk,
) *Writer {
	return &Writer{
		disk: disk,
		shm:  shm,

		in: make(
			chan writerCommand,
			writerQueueDepth,
		),
	}
}

func (w *Writer) Start() {
	w.wg.Add(1)

	go w.run()
}

func (w *Writer) Input() chan<- writerCommand {
	return w.in
}

func (w *Writer) SubmitWrite(
	ev WriteEvent,
) {
	w.in <- writerCommand{
		kind:  writerWrite,
		write: ev,
	}
}

func (w *Writer) SubmitPunch(
	req PunchRequest,
) {
	w.in <- writerCommand{
		kind:  writerPunch,
		punch: req,
	}
}

func (w *Writer) SubmitBarrier(
	done chan error,
) {
	w.in <- writerCommand{
		kind:    writerBarrier,
		barrier: done,
	}
}

func (w *Writer) Wait() {
	w.wg.Wait()
}

func (w *Writer) run() {
	defer w.wg.Done()

	for cmd := range w.in {
		switch cmd.kind {
		case writerWrite:
			w.processWrite(
				cmd.write,
			)

		case writerPunch:
			w.processPunch(
				cmd.punch,
			)

		case writerBarrier:
			err := w.disk.Sync()

			cmd.barrier <- err
			close(cmd.barrier)
		}
	}
}

func (w *Writer) processWrite(
	ev WriteEvent,
) {
	/*
		Record the write before touching the disk.

		This protects later punch operations against newer writes
		which have already entered the physical writer.
	*/
	w.rememberWrite(
		ev.Seq,
		ev.Offset,
		ev.Offset+
			uint64(len(ev.Data)),
	)

	n, err := w.disk.WriteAt(
		ev.Data,
		int64(ev.Offset),
	)
	if err != nil {
		log.Printf(
			"physical write failed seq=%d off=%d len=%d: %v",
			ev.Seq,
			ev.Offset,
			len(ev.Data),
			err,
		)

		return
	}

	if n != len(ev.Data) {
		log.Printf(
			"short physical write seq=%d off=%d n=%d want=%d",
			ev.Seq,
			ev.Offset,
			n,
			len(ev.Data),
		)

		return
	}

	if err := w.shm.ReleaseIfCurrent(
		[]ShmExtent{
			ev.Staged,
		},
	); err != nil {
		log.Printf(
			"SHM release seq=%d: %v",
			ev.Seq,
			err,
		)
	}
}

func (w *Writer) processPunch(
	req PunchRequest,
) {
	if req.End <= req.Start {
		return
	}

	/*
		A newer write already passed through the writer and touches
		the same physical range.

		Do NOT destroy it.
	*/
	if w.hasNewerOverlap(
		req.CaptureSeq,
		req.Start,
		req.End,
	) {
		if verbLog {
			log.Printf(
				"punch skipped due newer write captureSeq=%d phys=[%d,%d)",
				req.CaptureSeq,
				req.Start,
				req.End,
			)
		}

		return
	}

	err := syscall.Fallocate(
		int(w.disk.Fd()),
		fallocPunchHole|fallocKeepSize,
		int64(req.Start),
		int64(req.End-req.Start),
	)
	if err != nil {
		log.Printf(
			"punch failed captureSeq=%d phys=[%d,%d): %v",
			req.CaptureSeq,
			req.Start,
			req.End,
			err,
		)

		return
	}

	if verbLog {
		log.Printf(
			"punched captureSeq=%d phys=[%d,%d)",
			req.CaptureSeq,
			req.Start,
			req.End,
		)
	}
}

func (w *Writer) rememberWrite(
	seq,
	start,
	end uint64,
) {
	w.recent[w.recentHead] = WriteRecord{
		Seq:   seq,
		Start: start,
		End:   end,
	}

	w.recentHead =
		(w.recentHead + 1) %
			len(w.recent)

	if w.recentCount <
		len(w.recent) {
		w.recentCount++
	}
}

func (w *Writer) hasNewerOverlap(
	seq,
	start,
	end uint64,
) bool {
	for i := 0; i < w.recentCount; i++ {
		idx :=
			(w.recentHead -
				1 -
				i +
				len(w.recent)) %
				len(w.recent)

		r := w.recent[idx]

		if r.Seq <= seq {
			continue
		}

		if start < r.End &&
			r.Start < end {
			return true
		}
	}

	return false
}
