package main

import (
	"context"
	"sync"
)

const (
	hubQueueDepth = 4096

	clientInboxDepth = 4096

	clientOutDepth = 8192
)

type Client struct {
	inbox chan []byte
	out   chan []byte
	done  chan struct{}
}

func (c *Client) Ch() <-chan []byte {
	return c.out
}

type Hub struct {
	mu sync.RWMutex

	clients map[*Client]struct{}

	queue chan []byte

	done chan struct{}
}

func NewHub() *Hub {
	h := &Hub{
		clients: make(
			map[*Client]struct{},
		),

		queue: make(
			chan []byte,
			hubQueueDepth,
		),

		done: make(chan struct{}),
	}

	go h.dispatchLoop()

	return h
}

func (h *Hub) Broadcast(
	data []byte,
) {
	if len(data) == 0 {
		return
	}

	/*
		Never block the sniffer.

		If the hub is busy, discard the oldest packet and keep the
		newest packet moving.
	*/
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

func (h *Hub) Register(
	ctx context.Context,
) *Client {
	c := &Client{
		inbox: make(
			chan []byte,
			clientInboxDepth,
		),

		out: make(
			chan []byte,
			clientOutDepth,
		),

		done: make(chan struct{}),
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

func (h *Hub) Unregister(
	c *Client,
) {
	h.mu.Lock()

	_, ok := h.clients[c]

	if ok {
		delete(
			h.clients,
			c,
		)
	}

	h.mu.Unlock()

	if !ok {
		return
	}

	close(c.done)
}

func (h *Hub) Close() {
	close(h.done)
}

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

func (h *Hub) dispatch(
	data []byte,
) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for c := range h.clients {
		select {
		case c.inbox <- data:
			continue

		default:
		}

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
				}

				select {
				case <-c.out:
				default:
				}
			}

		next:
		}
	}
}
