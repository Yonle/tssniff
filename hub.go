package main

import (
	"errors"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type Hub struct {
	cmd         chan hubCommand
	clientCount int32 // Atomic counter to eliminate allocations when no clients exist

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

type hubCommand struct {
	op      hubOp
	client  *Client
	payload *sharedPayload
	done    chan struct{}
}

type hubOp uint8

const (
	hubAdd hubOp = iota
	hubRemove
	hubBroadcast
)

type Client struct {
	remoteAddr string
	q          chan *sharedPayload
	slowCount  int
}

type sharedPayload struct {
	data []byte
	refs atomic.Int32
}

func newSharedPayload(parts [][]byte) *sharedPayload {
	size := 0
	for _, part := range parts {
		size += len(part)
	}

	p := &sharedPayload{
		data: acquirePayload(size),
	}
	p.refs.Store(1)

	n := 0
	for _, part := range parts {
		n += copy(p.data[n:], part)
	}

	return p
}

func (p *sharedPayload) retain() {
	p.refs.Add(1)
}

func (p *sharedPayload) release() {
	if p.refs.Add(-1) == 0 {
		releasePayload(p.data)
	}
}

func NewHub() *Hub {
	h := &Hub{
		cmd:  make(chan hubCommand, 512),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	go h.run()
	return h
}

func (h *Hub) run() {
	clients := make(map[*Client]struct{})

	defer close(h.done)

	for {
		select {
		case <-h.stop:
			for client := range clients {
				h.closeClient(client)
			}
			atomic.StoreInt32(&h.clientCount, 0)
			return

		case cmd := <-h.cmd:
			switch cmd.op {
			case hubAdd:
				clients[cmd.client] = struct{}{}
				atomic.StoreInt32(&h.clientCount, int32(len(clients)))

				log.Printf(
					"TS HTTP client connected: %s (%d clients)",
					cmd.client.remoteAddr,
					len(clients),
				)

			case hubRemove:
				if _, ok := clients[cmd.client]; !ok {
					if cmd.done != nil {
						close(cmd.done)
					}
					continue
				}

				delete(clients, cmd.client)
				h.closeClient(cmd.client)
				atomic.StoreInt32(&h.clientCount, int32(len(clients)))

				log.Printf(
					"TS HTTP client disconnected: %s (%d clients)",
					cmd.client.remoteAddr,
					len(clients),
				)

			case hubBroadcast:
				if len(clients) == 0 {
					cmd.payload.release()
					break
				}

				p := cmd.payload

				for client := range clients {
					p.retain()

					select {
					case client.q <- p:
						client.slowCount = 0

					default:
						p.release()
						client.slowCount++

						if client.slowCount >= 10 {
							delete(clients, client)
							h.closeClient(client)
							atomic.StoreInt32(&h.clientCount, int32(len(clients)))

							log.Printf(
								"TS HTTP client dropped: %s (slow, %d clients remaining)",
								client.remoteAddr,
								len(clients),
							)
						}
					}
				}

				p.release()
			}

			if cmd.done != nil {
				close(cmd.done)
			}
		}
	}
}

func (h *Hub) submit(cmd hubCommand) bool {
	select {
	case h.cmd <- cmd:
	case <-h.stop:
		return false
	case <-h.done:
		return false
	}

	if cmd.done == nil {
		return true
	}

	select {
	case <-cmd.done:
		return true
	case <-h.stop:
		return false
	case <-h.done:
		return false
	}
}

func (h *Hub) Serve(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/stream", h.handleStream)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		WriteTimeout:      0,
	}

	go func() {
		<-h.stop
		_ = server.Close()
	}()

	log.Printf("TS HTTP server listening on http://%s/stream", addr)

	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}

	return err
}

func (h *Hub) handleStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(
			w,
			http.StatusText(http.StatusMethodNotAllowed),
			http.StatusMethodNotAllowed,
		)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(
			w,
			"streaming unsupported",
			http.StatusInternalServerError,
		)
		return
	}

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")

	client := &Client{
		remoteAddr: r.RemoteAddr,
		q:          make(chan *sharedPayload, 1024),
	}

	addDone := make(chan struct{})
	if !h.submit(hubCommand{
		op:     hubAdd,
		client: client,
		done:   addDone,
	}) {
		http.Error(
			w,
			"stream server shutting down",
			http.StatusServiceUnavailable,
		)
		return
	}

	defer func() {
		removeDone := make(chan struct{})

		_ = h.submit(hubCommand{
			op:     hubRemove,
			client: client,
			done:   removeDone,
		})
	}()

	flusher.Flush()

	writeAndRelease := func(p *sharedPayload) error {
		_, err := w.Write(p.data)
		p.release()
		return err
	}

	for {
		select {
		case <-r.Context().Done():
			return

		case payload, ok := <-client.q:
			if !ok {
				return
			}

			if err := writeAndRelease(payload); err != nil {
				return
			}

		drainLoop:
			for {
				select {
				case p, ok := <-client.q:
					if !ok {
						flusher.Flush()
						return
					}
					if err := writeAndRelease(p); err != nil {
						return
					}
				default:
					break drainLoop
				}
			}

			flusher.Flush()
		}
	}
}

func (h *Hub) closeClient(client *Client) {
	close(client.q)

	for payload := range client.q {
		payload.release()
	}
}

// BroadcastBatch captures the kernel buffer synchronously before returning to FUSE
func (h *Hub) BroadcastBatch(parts [][]byte) {
	if len(parts) == 0 {
		return
	}

	// Zero-allocation shortcut: if no clients are connected, exit instantly
	if atomic.LoadInt32(&h.clientCount) == 0 {
		return
	}

	// Copy data synchronously ON the FUSE thread while parts is guaranteed valid
	p := newSharedPayload(parts)

	cmd := hubCommand{
		op:      hubBroadcast,
		payload: p,
	}

	select {
	case h.cmd <- cmd:
		// Queued successfully
	default:
		// Queue backed up: drop payload and release memory to protect FUSE
		p.release()
		log.Printf("TS HTTP hub queue full, dropping broadcast payload")
	}
}

func (h *Hub) Close() {
	h.stopOnce.Do(func() {
		close(h.stop)
	})

	<-h.done
}