// hub.go
package main

import (
	"context"
	"sync"
	"time"
)

const (
	// Hub queue: payloads sitting between the FUSE worker and the
	// dispatch goroutine.  When full, the oldest is dropped.
	hubQueueDepth = 4096

	// Per-client inbox: payloads queued for a client's own drain
	// goroutine.  When full, the oldest is dropped for that client only.
	clientInboxDepth = 4096

	// Per-client out channel: what the HTTP handler reads from.
	// When full, the oldest is dropped by the drain goroutine.
	clientOutDepth = 8192

	// How long Broadcast blocks before giving up and dropping the
	// oldest item. This is the knob that trades request latency for
	// stream integrity. Set to 0 to restore the old drop-immediately
	// behaviour.
	broadcastBackpressure = 250 * time.Millisecond
)

type Client struct {
	inbox chan []byte
	out   chan []byte
	done  chan struct{}
}

func (c *Client) Ch() <-chan []byte { return c.out }

type Hub struct {
	mu      sync.RWMutex
	clients map[*Client]struct{}

	queue chan []byte
	done  chan struct{}
}

func NewHub() *Hub {
	h := &Hub{
		clients: make(map[*Client]struct{}),
		queue:   make(chan []byte, hubQueueDepth),
		done:    make(chan struct{}),
	}
	go h.dispatchLoop()
	return h
}

// Broadcast never blocks indefinitely. It first tries to enqueue;
// if the hub queue is full, it applies bounded backpressure before
// falling back to drop-oldest.
func (h *Hub) Broadcast(data []byte) {
	if len(data) == 0 {
		return
	}

	// Fast path: queue has room.
	select {
	case h.queue <- data:
		return
	default:
	}

	// Bounded backpressure: give the dispatcher a chance to drain
	// before we discard anything.
	if broadcastBackpressure > 0 {
		t := time.NewTimer(broadcastBackpressure)

		select {
		case h.queue <- data:
			t.Stop()
			return

		case <-h.done:
			t.Stop()
			return

		case <-t.C:
		}
	}

	// Still full. Drop the oldest item for the newest one.
	select {
	case <-h.queue:
	default:
	}
	select {
	case h.queue <- data:
	default:
	}
}

func (h *Hub) Register(ctx context.Context) *Client {
	c := &Client{
		inbox: make(chan []byte, clientInboxDepth),
		out:   make(chan []byte, clientOutDepth),
		done:  make(chan struct{}),
	}

	go c.drain()

	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	go func() {
		<-ctx.Done()
		h.Unregister(c)
	}()

	return c
}

func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	had := false
	if _, exists := h.clients[c]; exists {
		delete(h.clients, c)
		had = true
	}
	h.mu.Unlock()

	if !had {
		return
	}

	close(c.done)
}

func (h *Hub) Close() {
	close(h.done)
}

// drainLoop is the only goroutine that reads h.queue.
func (h *Hub) dispatchLoop() {
	for {
		select {
		case <-h.done:
			return
		case data := <-h.queue:
			h.dispatch(data)
		}
	}
}

// dispatch does one non-blocking enqueue per client.  A client whose
// inbox is full gets its oldest entry dropped; the loop still moves on
// to the next client in O(1).
func (h *Hub) dispatch(data []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for c := range h.clients {
		select {
		case c.inbox <- data:
		default:
			// Inbox full.  Drop oldest for this client only.
			select {
			case <-c.inbox:
			default:
			}
			select {
			case c.inbox <- data:
			default:
			}
		}
	}
}

// drain is the only goroutine that writes to c.out.
// It moves items from c.inbox to c.out, dropping the oldest from c.out
// when the HTTP handler is not reading fast enough.
func (c *Client) drain() {
	defer close(c.out)

	for {
		select {
		case <-c.done:
			return
		case data := <-c.inbox:
			for {
				select {
				case c.out <- data:
					goto next
				default:
					select {
					case <-c.out:
					default:
					}
				}
			}
		next:
		}
	}
}
