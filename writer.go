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
	writerBarrier
)

type writerCommand struct {
	kind writerCommandKind

	write   ObservedWrite
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

	/*
		Normal commands:
		- writes
		- barriers

		These always have priority over punches.
	*/
	in chan writerCommand

	/*
		Punches are deliberately kept separate from normal writes.

		A slow FALLOC_FL_PUNCH_HOLE therefore cannot fill the normal
		writer queue or force us to interleave write/punch commands
		inside one channel.
	*/
	punchIn chan PunchRequest

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

		punchIn: make(
			chan PunchRequest,
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

func (w *Writer) PunchInput() chan<- PunchRequest {
	return w.punchIn
}

func (w *Writer) SubmitWrite(
	ev ObservedWrite,
) {
	w.in <- writerCommand{
		kind:  writerWrite,
		write: ev,
	}
}

func (w *Writer) SubmitPunch(
	req PunchRequest,
) bool {
	select {
	case w.punchIn <- req:
		return true

	default:
		if verbLog {
			log.Printf(
				"punch queue full, deferred captureSeq=%d phys=[%d,%d)",
				req.CaptureSeq,
				req.Start,
				req.End,
			)
		}

		return false
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

func (w *Writer) CloseInput() {
	close(w.in)
}

func (w *Writer) ClosePunchInput() {
	close(w.punchIn)
}

func (w *Writer) Wait() {
	w.wg.Wait()
}

func (w *Writer) run() {
	defer w.wg.Done()

	in := w.in
	punchIn := w.punchIn

	for {
		/*
			Drain normal commands whenever one is immediately
			available.

			This gives writes/barriers priority over punches.
		*/
		for in != nil {
			select {
			case cmd, ok := <-in:
				if !ok {
					in = nil
					continue
				}

				w.processCommand(cmd)

			default:
				/*
					No normal command is immediately available.
				*/
				goto wait
			}
		}

	wait:
		/*
			If both inputs are closed, we're done.
		*/
		if in == nil &&
			punchIn == nil {
			return
		}

		/*
			Wait for either:
			- another normal command
			- a punch

			Normal commands win whenever they are already queued because
			the loop above drains them before entering this select.
		*/
		switch {
		case in != nil && punchIn != nil:
			select {
			case cmd, ok := <-in:
				if !ok {
					in = nil
					continue
				}

				w.processCommand(cmd)

			case req, ok := <-punchIn:
				if !ok {
					punchIn = nil
					continue
				}

				w.processPunch(req)
			}

		case in != nil:
			cmd, ok := <-in
			if !ok {
				in = nil
				continue
			}

			w.processCommand(cmd)

		default:
			req, ok := <-punchIn
			if !ok {
				punchIn = nil
				continue
			}

			w.processPunch(req)
		}
	}
}

func (w *Writer) processCommand(
	cmd writerCommand,
) {
	switch cmd.kind {
	case writerWrite:
		w.processWrite(
			cmd.write,
		)

	case writerBarrier:
		err := w.disk.Sync()

		cmd.barrier <- err
		close(cmd.barrier)
	}
}

func (w *Writer) processWrite(
	ev ObservedWrite,
) {
	write := ev.Event

	end :=
		write.Offset +
			uint64(len(write.Data))

	if ev.MPEGTS {
		/*
			The sniffer has already processed this write.

			The STB never reads MPEG-TS payload back, so there is
			no reason to materialize it in the sparse backing image.
		*/
		if err := w.shm.ReleaseIfCurrent(
			[]ShmExtent{
				write.Staged,
			},
		); err != nil {
			log.Printf(
				"SHM release TS seq=%d: %v",
				write.Seq,
				err,
			)
		}

		return
	}

	w.rememberWrite(
		write.Seq,
		write.Offset,
		end,
	)

	n, err := w.disk.WriteAt(
		write.Data,
		int64(write.Offset),
	)
	if err != nil {
		log.Printf(
			"physical write failed seq=%d off=%d len=%d: %v",
			write.Seq,
			write.Offset,
			len(write.Data),
			err,
		)

		return
	}

	if n != len(write.Data) {
		log.Printf(
			"short physical write seq=%d off=%d n=%d want=%d",
			write.Seq,
			write.Offset,
			n,
			len(write.Data),
		)

		return
	}

	if err := w.shm.ReleaseIfCurrent(
		[]ShmExtent{
			write.Staged,
		},
	); err != nil {
		log.Printf(
			"SHM release seq=%d: %v",
			write.Seq,
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
