// hub.go
package main

import (
	"context"
	"sync"
)

const (
	// Hub queue: payloads sitting between the FUSE worker and the
	// dispatch goroutine.  When full, the oldest is dropped.
	hubQueueDepth = 256

	// Per-client inbox: payloads queued for a client's own drain
	// goroutine.  When full, the oldest is dropped for that client only.
	clientInboxDepth = 256

	// Per-client out channel: what the HTTP handler reads from.
	// When full, the oldest is dropped by the drain goroutine.
	clientOutDepth = 512
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

// Broadcast never blocks.  Enqueue onto the hub queue and return.
// If the queue is full, the oldest item is dropped to make room.
func (h *Hub) Broadcast(data []byte) {
	select {
	case h.queue <- data:
		return
	default:
	}

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
