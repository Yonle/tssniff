package main

import (
	"log"
	"net"
	"net/http"
)

func startStreamServer(addr string, hub *Hub) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("TCP listener failed on %s: %v", addr, err)
	}
	defer listener.Close()

	log.Printf("TS Stream Server listening on %s", addr)

	mux := http.NewServeMux()
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Cache-Control", "no-cache")

		flusher, _ := w.(http.Flusher)

		// Send headers now.  Without this, Go buffers them until the
		// first body write, and a client that connects while no TS
		// data is being broadcast sees a connection that hangs.
		if flusher != nil {
			flusher.Flush()
		}

		client := hub.Register(r.Context())
		defer hub.Unregister(client)

		for data := range client.Ch() {
			if _, err := w.Write(data); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	})

	srv := &http.Server{Handler: mux}
	_ = srv.Serve(listener)
}
