package main

import (
	"log"
	"sync"
	"sync/atomic"
	"syscall"
)

const (
	pipelineWriteDepth   = 2048
	pipelinePunchDepth   = 8192
	pipelineBarrierDepth = 8
)

type barrierRequest struct {
	TargetSeq uint64
	Done      chan error
}

type WritePipeline struct {
	writes chan WriteEvent

	punches chan PunchRequest

	barriers chan barrierRequest

	writer  *Writer
	sniffer *Sniffer

	closing atomic.Bool

	/*
		Only used for fallback senders created when writes is full.
	*/
	submitWG sync.WaitGroup

	wg sync.WaitGroup
}

func NewWritePipeline(
	writer *Writer,
	sniffer *Sniffer,
) *WritePipeline {
	return &WritePipeline{
		writes: make(
			chan WriteEvent,
			pipelineWriteDepth,
		),

		punches: make(
			chan PunchRequest,
			pipelinePunchDepth,
		),

		barriers: make(
			chan barrierRequest,
			pipelineBarrierDepth,
		),

		writer: writer,

		sniffer: sniffer,
	}
}

func (p *WritePipeline) Start() {
	/*
		One goroutine owns normal write/barrier dispatch.

		Another goroutine forwards punches separately so a blocked
		punch can never block normal write dispatch.
	*/
	p.wg.Add(2)

	go p.run()
	go p.runPunches()
}

func (p *WritePipeline) SubmitWrite(
	ev WriteEvent,
) error {
	if p.closing.Load() {
		return syscall.EIO
	}

	/*
		Fast path.

		This is what virtually every normal Write() uses.
	*/
	select {
	case p.writes <- ev:
		return nil

	default:
	}

	/*
		The FUSE Write() MUST NOT wait for the sniffer/writer pipeline.

		Move the blocking send to another goroutine.
	*/
	p.submitWG.Add(1)

	go func() {
		defer p.submitWG.Done()

		p.writes <- ev
	}()

	return nil
}

func (p *WritePipeline) SubmitPunch(
	req PunchRequest,
) bool {
	/*
		Punches must never block the reconciler.

		If the bounded queue is full, leave the capture in the
		reconciler's journal and let a later reconciliation attempt
		submit it again.
	*/
	select {
	case p.punches <- req:
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

func (p *WritePipeline) Sync(
	targetSeq uint64,
) error {
	/*
		Ensure every fallback Write() sender has entered p.writes.
	*/
	p.submitWG.Wait()

	done := make(chan error, 1)

	p.barriers <- barrierRequest{
		TargetSeq: targetSeq,
		Done:      done,
	}

	return <-done
}

func (p *WritePipeline) CloseWrites() {
	/*
		Call only after FUSE has been unmounted, so no new Write()
		can enter this pipeline.
	*/
	p.closing.Store(true)

	p.submitWG.Wait()

	close(p.writes)
}

func (p *WritePipeline) ClosePunches() {
	close(p.punches)
}

func (p *WritePipeline) Wait() {
	p.wg.Wait()
}

func (p *WritePipeline) run() {
	defer p.wg.Done()

	pending := make(
		map[uint64]WriteEvent,
	)

	var nextSeq uint64 = 1
	var dispatchedSeq uint64

	writesOpen := true
	snifferClosed := false

	/*
		dispatchWrite is the ONLY place that forwards a write to the
		writer and sniffer.

		Therefore they observe exactly the same sequence.
	*/
	dispatchWrite := func(ev WriteEvent) {
		p.writer.SubmitWrite(ev)
		p.sniffer.Input() <- ev

		dispatchedSeq = ev.Seq
	}

	/*
		Accept an arbitrary-arrival WriteEvent and release all
		consecutive sequence numbers that are now available.
	*/
	acceptWrite := func(ev WriteEvent) {
		if ev.Seq < nextSeq {
			return
		}

		if ev.Seq > nextSeq {
			pending[ev.Seq] = ev
			return
		}

		for {
			dispatchWrite(ev)

			nextSeq++

			next, ok := pending[nextSeq]

			if !ok {
				return
			}

			delete(
				pending,
				nextSeq,
			)

			ev = next
		}
	}

	/*
		Barrier handling.

		By definition, the sniffer cannot have observed TargetSeq
		until the dispatcher already dispatched it. But Flush/Fsync
		may race with fallback write delivery, so explicitly drain
		until the target has been dispatched.
	*/
	handleBarrier := func(req barrierRequest) {
		for dispatchedSeq <
			req.TargetSeq {

			if !writesOpen {
				break
			}

			ev, ok := <-p.writes

			if !ok {
				writesOpen = false
				break
			}

			acceptWrite(ev)
		}

		p.writer.SubmitBarrier(
			req.Done,
		)
	}

	for writesOpen {
		select {
		case ev, ok := <-p.writes:

			if !ok {
				writesOpen = false

				if !snifferClosed {
					close(
						p.sniffer.Input(),
					)

					snifferClosed = true
				}

				continue
			}

			acceptWrite(ev)

		case req := <-p.barriers:

			handleBarrier(req)
		}
	}

	/*
		All normal writes have been dispatched.

		If writes remained in the reorder map, something violated the
		FUSE submission sequence. Don't silently pretend everything
		is okay.
	*/
	if len(pending) != 0 {
		log.Printf(
			"write pipeline stopped with %d pending out-of-order writes",
			len(pending),
		)
	}

	if !snifferClosed {
		close(
			p.sniffer.Input(),
		)
	}

	/*
		Normal writer commands are now finished.

		The punch-forwarder owns the writer's punch input separately.
	*/
	p.writer.CloseInput()
}

func (p *WritePipeline) runPunches() {
	defer p.wg.Done()

	/*
		This goroutine is deliberately allowed to block on
		writer.SubmitPunch().

		It is isolated from the normal write pipeline, so a slow
		eMMC punch cannot block WriteEvent dispatch.
	*/
	for req := range p.punches {
		p.writer.SubmitPunch(req)
	}

	/*
		No more punches can be generated after this point.
	*/
	p.writer.ClosePunchInput()
}
