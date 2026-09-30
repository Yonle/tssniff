package main

import (
	"log"
	"sync"
)

const snifferQueueDepth = 1024

type Sniffer struct {
	in chan WriteEvent

	out chan ObservedWrite

	hub *Hub

	scanner *TSScanner

	wg sync.WaitGroup
}

func NewSniffer(
	hub *Hub,
) *Sniffer {
	return &Sniffer{
		in: make(
			chan WriteEvent,
			snifferQueueDepth,
		),

		out: make(
			chan ObservedWrite,
			snifferQueueDepth,
		),

		hub: hub,

		scanner: NewTSScanner(),
	}
}

func (s *Sniffer) Start() {
	s.wg.Add(1)

	go s.run()
}

func (s *Sniffer) Input() chan<- WriteEvent {
	return s.in
}

func (s *Sniffer) Output() <-chan ObservedWrite {
	return s.out
}

func (s *Sniffer) Wait() {
	s.wg.Wait()
}

func (s *Sniffer) run() {
	defer s.wg.Done()
	defer close(s.out)

	for ev := range s.in {
		chunks, _ := s.scanner.Feed(
			ev.Offset,
			ev.Data,
		)

		var captures []CaptureRange

		for _, chunk := range chunks {
			/*
				This chunk is already an owned copy from TSScanner.

				Hub.Broadcast itself is non-blocking.
			*/
			s.hub.Broadcast(
				chunk.Data,
			)

			captures = append(
				captures,
				CaptureRange{
					Seq:   ev.Seq,
					Start: chunk.Start,
					End:   chunk.End,
				},
			)

			if verbLog {
				log.Printf(
					"MPEG-TS capture seq=%d phys=[%d,%d)",
					ev.Seq,
					chunk.Start,
					chunk.End,
				)
			}
		}

		s.out <- ObservedWrite{
			Seq: ev.Seq,

			Offset: ev.Offset,
			End: ev.Offset +
				uint64(len(ev.Data)),

			TouchesMFT: ev.TouchesMFT,

			Captures: captures,
		}
	}
}
