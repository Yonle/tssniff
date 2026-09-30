package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fuse"
)

var verbLog bool

func main() {
	mountPoint := flag.String(
		"mount",
		"/mnt/tsdisk",
		"FUSE mount point",
	)

	imagePath := flag.String(
		"image",
		"/srv/guoxin.img",
		"sparse backing image",
	)

	listenAddr := flag.String(
		"listen",
		":6969",
		"HTTP TS stream listener",
	)

	debug := flag.Bool(
		"debug",
		false,
		"FUSE debug",
	)

	noGadget := flag.Bool(
		"no-gadget",
		false,
		"skip USB gadget setup",
	)

	flag.BoolVar(
		&verbLog,
		"verbose",
		false,
		"verbose logging",
	)

	flag.Parse()

	imgFile, err := os.OpenFile(
		*imagePath,
		os.O_RDWR,
		0o666,
	)
	if err != nil {
		log.Fatalf(
			"open sparse image %s: %v",
			*imagePath,
			err,
		)
	}

	defer imgFile.Close()

	st, err := imgFile.Stat()
	if err != nil {
		log.Fatalf(
			"stat sparse image: %v",
			err,
		)
	}

	if !st.Mode().IsRegular() {
		log.Fatalf(
			"%s is not a regular file",
			*imagePath,
		)
	}

	size := uint64(st.Size())

	part, err := findMBRPartition(
		imgFile,
	)
	if err != nil {
		log.Fatalf(
			"find NTFS partition: %v",
			err,
		)
	}

	if verbLog {
		log.Printf(
			"NTFS partition offset=%d size=%d",
			part.Offset,
			part.Size,
		)
	}

	/*
		SHM is the authoritative acknowledged-write layer.

		NTFS also reads through this object, so MFT parsing sees bytes
		which FUSE has already acknowledged even if the physical writer
		has not reached them yet.
	*/
	shm, err := NewSHMDisk(
		imgFile,
		size,
	)
	if err != nil {
		log.Fatalf(
			"initialize SHM: %v",
			err,
		)
	}

	/*
		NTFS has no worker goroutine of its own.

		The Reconciler owns all mutable NTFS state and is the only
		goroutine which calls ObserveMFTWrite().
	*/
	ntfs, err := NewNTFS(
		part,
		shm,
	)
	if err != nil {
		_ = shm.Close()

		log.Fatalf(
			"initialize NTFS tracker: %v",
			err,
		)
	}

	hub := NewHub()

	writer := NewWriter(
		imgFile,
		shm,
	)

	sniffer := NewSniffer(
		hub,
	)

	pipeline := NewWritePipeline(
		writer,
		sniffer,
	)

	reconciler := NewReconciler(
		sniffer.Output(),
		ntfs,
		pipeline,
	)

	if err := os.MkdirAll(
		*mountPoint,
		0o755,
	); err != nil {

		_ = shm.Close()

		log.Fatalf(
			"mkdir %s: %v",
			*mountPoint,
			err,
		)
	}

	server, _, err := mountDiskFS(
		*mountPoint,
		imgFile,
		shm,
		writer,
		pipeline,
		ntfs,
		*debug,
	)
	if err != nil {
		_ = shm.Close()

		log.Fatalf(
			"mount FUSE: %v",
			err,
		)
	}

	diskPath := filepath.Join(
		*mountPoint,
		"disk.img",
	)

	log.Printf(
		"FUSE disk: %s",
		diskPath,
	)

	/*
		Start the workers only after FUSE mounted successfully.
	*/
	writer.Start()
	sniffer.Start()
	reconciler.Start()
	pipeline.Start()

	httpServer,
		httpListener,
		err := newStreamServer(
		*listenAddr,
		hub,
	)
	if err != nil {
		if unmountErr := server.Unmount(); unmountErr != nil {
			log.Printf(
				"FUSE unmount: %v",
				unmountErr,
			)
		}

		pipeline.CloseWrites()

		sniffer.Wait()
		reconciler.Wait()

		pipeline.ClosePunches()
		pipeline.Wait()
		writer.Wait()

		_ = shm.Close()

		log.Fatalf(
			"start HTTP server: %v",
			err,
		)
	}

	go serveStreamServer(
		httpServer,
		httpListener,
	)

	log.Printf(
		"Stream: http://localhost%s/stream",
		*listenAddr,
	)

	var gadget *USBGadget

	if !*noGadget {
		gadget = NewUSBGadget(
			diskPath,
		)

		if err := gadget.Setup(); err != nil {

			shutdown(
				server,
				gadget,
				pipeline,
				sniffer,
				reconciler,
				writer,
				httpServer,
				hub,
				shm,
			)

			log.Fatalf(
				"USB gadget setup: %v",
				err,
			)
		}

		log.Printf(
			"USB gadget active, backing %s",
			diskPath,
		)
	} else {
		log.Printf(
			"USB gadget skipped",
		)
	}

	signalCh := make(chan os.Signal, 1)

	signal.Notify(
		signalCh,
		os.Interrupt,
		syscall.SIGTERM,
	)

	<-signalCh

	log.Println(
		"Shutting down...",
	)

	shutdown(
		server,
		gadget,
		pipeline,
		sniffer,
		reconciler,
		writer,
		httpServer,
		hub,
		shm,
	)
}

func shutdown(
	server *fuse.Server,
	gadget *USBGadget,
	pipeline *WritePipeline,
	sniffer *Sniffer,
	reconciler *Reconciler,
	writer *Writer,
	httpServer *http.Server,
	hub *Hub,
	shm *SHMDisk,
) {
	/*
		1. Stop the USB consumer.
	*/
	if gadget != nil {
		gadget.Teardown()
	}

	/*
		2. Stop accepting new FUSE requests.

		Unmount first. After it returns, Write() cannot begin another
		request, which makes it safe for CloseWrites() to wait for the
		rare fallback senders.
	*/
	if err := server.Unmount(); err != nil {
		log.Printf(
			"FUSE unmount: %v",
			err,
		)
	}

	/*
		3. Close the FUSE write input after all already-accepted
		   fallback sends have reached the pipeline.
	*/
	pipeline.CloseWrites()

	/*
		4. Wait until the sniffer has observed every accepted write.
	*/
	sniffer.Wait()

	/*
		5. The reconciler now has the complete observation stream.

		Waiting here ensures every MFT-triggered punch has been
		submitted before the punch source is closed.
	*/
	reconciler.Wait()

	/*
		6. No more punch requests can be generated.
	*/
	pipeline.ClosePunches()

	/*
		7. Let the dispatcher finish and close the writer input.
	*/
	pipeline.Wait()

	/*
		8. Drain the physical writer.
	*/
	writer.Wait()

	/*
		9. Final backing-file flush.
	*/
	_ = osSyncBacking(writer)

	/*
		10. Stop HTTP after no more TS will be generated.
	*/
	ctx,
		cancel := context.WithTimeout(
		context.Background(),
		2*time.Second,
	)

	_ = httpServer.Shutdown(ctx)

	cancel()

	hub.Close()

	/*
		11. Only now is it safe to destroy the SHM overlay.
	*/
	if err := shm.Close(); err != nil {
		log.Printf(
			"SHM close: %v",
			err,
		)
	}
}

func osSyncBacking(
	writer *Writer,
) error {
	if writer == nil ||
		writer.disk == nil {
		return nil
	}

	return writer.disk.Sync()
}
