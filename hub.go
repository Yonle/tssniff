// hub.go
package main

import (
	"context"
	"sync"
)

const clientQueueDepth = 64

type Client struct {
	ch chan []byte
}

// Ch exposes the receive side of the client channel.
func (c *Client) Ch() <-chan []byte { return c.ch }

type Hub struct {
	clients map[*Client]struct{}
	mu      sync.RWMutex
}

func NewHub() *Hub {
	return &Hub{clients: make(map[*Client]struct{})}
}

func (h *Hub) Register(ctx context.Context) *Client {
	c := &Client{ch: make(chan []byte, clientQueueDepth)}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	go func() {
		<-ctx.Done()
		h.unregister(c)
	}()
	return c
}

// Unregister removes c from the hub immediately.  Safe to call multiple times.
func (h *Hub) Unregister(c *Client) {
	h.unregister(c)
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.clients[c]; exists {
		delete(h.clients, c)
		close(c.ch)
	}
}

func (h *Hub) Broadcast(data []byte) {
	h.mu.RLock()
	var slow []*Client
	for c := range h.clients {
		select {
		case c.ch <- data:
		default:
			slow = append(slow, c)
		}
	}
	h.mu.RUnlock()

	if len(slow) == 0 {
		return
	}

	h.mu.Lock()
	for _, c := range slow {
		if _, exists := h.clients[c]; exists {
			delete(h.clients, c)
			close(c.ch)
		}
	}
	h.mu.Unlock()
}
