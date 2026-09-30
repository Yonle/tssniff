package main

import (
	"log"
	"net"
	"net/http"
)

func newStreamServer(
	addr string,
	hub *Hub,
) (*http.Server, net.Listener, error) {
	listener, err := net.Listen(
		"tcp",
		addr,
	)
	if err != nil {
		return nil,
			nil,
			err
	}

	mux := http.NewServeMux()

	mux.HandleFunc(
		"/stream",
		func(
			w http.ResponseWriter,
			r *http.Request,
		) {
			w.Header().Set(
				"Content-Type",
				"video/mp2t",
			)

			w.Header().Set(
				"Cache-Control",
				"no-cache",
			)

			flusher, _ := w.(http.Flusher)

			if flusher != nil {
				flusher.Flush()
			}

			client := hub.Register(
				r.Context(),
			)

			defer hub.Unregister(
				client,
			)

			for data := range client.Ch() {

				if _, err := w.Write(data); err != nil {
					return
				}

				if flusher != nil {
					flusher.Flush()
				}
			}
		},
	)

	server := &http.Server{
		Handler: mux,
	}

	return server,
		listener,
		nil
}

func serveStreamServer(
	server *http.Server,
	listener net.Listener,
) {
	log.Printf(
		"TS Stream Server listening on %s",
		listener.Addr(),
	)

	err := server.Serve(listener)

	if err != nil &&
		err != http.ErrServerClosed {
		log.Printf(
			"stream server stopped: %v",
			err,
		)
	}
}
