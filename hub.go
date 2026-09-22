package main

import (
	"context"
	"sync"
)

const clientQueueDepth = 512

type Client struct {
	ch chan []byte
}

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
		h.Unregister(c)
	}()

	return c
}

func (h *Hub) Unregister(c *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, exists := h.clients[c]; exists {
		delete(h.clients, c)
		close(c.ch)
	}
}

func (h *Hub) Broadcast(data []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for c := range h.clients {
		for {
			select {
			case c.ch <- data:
				goto next
			default:
				select {
				case <-c.ch:
				default:
				}
			}
		}
	next:
	}
}
