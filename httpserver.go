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
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("Cache-Control", "no-cache")

		flusher, _ := w.(http.Flusher)

		client := hub.Register(r.Context())
		defer hub.Unregister(client)

		for data := range client.ch {
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
