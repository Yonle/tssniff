package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
)

var verbLog bool

func main() {
	image := flag.String("image", "/srv/guoxin.img", "sparse backing image for metadata")
	listenAddr := flag.String("listen", ":6969", "TCP TS stream listener")
	filesystem := flag.String("fs", "fat32", "filesystem type")
	preserve := flag.Bool("preserve", false, "preserve MPEG-TS data to /dev/shm")
	nbdDev := flag.String("nbd", "/dev/nbd0", "NBD device path")
	noGadget := flag.Bool("no-gadget", false, "skip USB gadget setup (for local testing against /dev/nbd0)")
	flag.BoolVar(&verbLog, "verbose", false, "verbose logging")
	flag.Parse()

	// 1. Open the sparse backing image (metadata only).
	imgFile, err := os.OpenFile(*image, os.O_RDWR, 0666)
	if err != nil {
		log.Fatalf("open sparse image %s: %v", *image, err)
	}
	defer imgFile.Close()

	imgInfo, err := imgFile.Stat()
	if err != nil {
		log.Fatalf("stat sparse image: %v", err)
	}

	// 2. Parse MBR to find partition boundaries.
	part, err := findMBRPartition(imgFile, *filesystem)
	if err != nil {
		log.Printf("Warning: MBR partition check: %v (raw device mode)", err)
	} else if verbLog {
		log.Printf("Found %s partition at offset %d, size %d",
			*filesystem, part.Offset, part.Size)
	}

	// 3. Initialize Hub and Tracker.
	hub := NewHub()
	tracker := NewFSTracker(*filesystem, part, imgFile)

	// 4. /dev/shm buffer (only when preserve is enabled).
	var shm *ShmBuffer
	if *preserve {
		shm, err = NewShmBuffer()
		if err != nil {
			log.Fatalf("create /dev/shm buffer: %v", err)
		}
		defer shm.Close()
		log.Printf("Preserve enabled, using %s", shm.Path())
	}

	// 5. Start HTTP TS stream server.
	go startStreamServer(*listenAddr, hub)

	// 6. Create NBD backend and server.
	backend := NewNBDBackend(imgFile, imgInfo.Size(), hub, tracker, *preserve, shm)
	nbdSrv := NewNBDServer(backend, *nbdDev)
	if err := nbdSrv.Start(); err != nil {
		log.Fatalf("start NBD server: %v", err)
	}
	defer nbdSrv.Stop()

	// 7. Set up USB gadget with the NBD device as backing (optional).
	if !*noGadget {
		gadget := NewUSBGadget(*nbdDev)
		if err := gadget.Setup(); err != nil {
			log.Fatalf("USB gadget setup: %v", err)
		}
		defer gadget.Teardown()
		log.Printf("USB gadget active. STB can now access the virtual disk.")
	} else {
		log.Printf("USB gadget skipped (-no-gadget). %s is ready for local testing.", *nbdDev)
	}

	log.Printf("TSSniff running. Stream: http://localhost%s/stream", *listenAddr)

	// 8. Wait for signal.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	log.Println("Shutting down...")
	backend.Flush()
}
