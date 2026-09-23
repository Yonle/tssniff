package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

var verbLog bool

func main() {
	mountPoint := flag.String("mount", "/mnt/tsdisk", "FUSE mount point")
	image := flag.String("image", "/dev/shm/guoxin.img", "sparse backing image")
	listenAddr := flag.String("listen", ":6969", "HTTP stream listener")
	filesystem := flag.String("fs", "fat32", "filesystem type")
	debug := flag.Bool("debug", false, "FUSE debug")
	preserve := flag.Bool("preserve", false, "preserve TS to /dev/shm")
	noGadget := flag.Bool("no-gadget", false, "skip USB gadget setup")
	flag.BoolVar(&verbLog, "verbose", false, "verbose logging")
	flag.Parse()

	imgFile, err := os.OpenFile(*image, os.O_RDWR, 0666)
	if err != nil {
		log.Fatalf("open sparse image %s: %v", *image, err)
	}
	defer imgFile.Close()

	st, err := imgFile.Stat()
	if err != nil {
		log.Fatalf("stat sparse image: %v", err)
	}
	if !st.Mode().IsRegular() {
		log.Fatalf("%s is not a regular file", *image)
	}

	part, err := findMBRPartition(imgFile, *filesystem)
	if err != nil {
		log.Printf("Warning: MBR partition check: %v (raw device mode)", err)
	} else if verbLog {
		log.Printf("Found %s partition at offset %d, size %d",
			*filesystem, part.Offset, part.Size)
	}

	hub := NewHub()
	tracker := NewFSTracker(*filesystem, part, imgFile)

	if verbLog {
		log.Printf("tracker: metaEnd=%d", tracker.MetadataEnd())
	}

	var shm *ShmBuffer
	if *preserve {
		shm, err = NewShmBuffer()
		if err != nil {
			log.Fatalf("create /dev/shm buffer: %v", err)
		}
		defer shm.Close()
		log.Printf("Preserve enabled, using %s", shm.Path())
	}

	go startStreamServer(*listenAddr, hub)

	if err := os.MkdirAll(*mountPoint, 0755); err != nil {
		log.Fatalf("mkdir %s: %v", *mountPoint, err)
	}

	server, err := mountDiskFS(DiskFSOpts{
		MountPoint: *mountPoint,
		Image:      imgFile,
		Hub:        hub,
		Tracker:    tracker,
		Preserve:   *preserve,
		Shm:        shm,
		Debug:      *debug,
	})
	if err != nil {
		log.Fatalf("mount FUSE: %v", err)
	}

	diskPath := filepath.Join(*mountPoint, "disk.img")
	log.Printf("FUSE disk: %s", diskPath)
	log.Printf("Stream:    http://localhost%s/stream", *listenAddr)

	var gadget *USBGadget
	if !*noGadget {
		gadget = NewUSBGadget(diskPath)
		if err := gadget.Setup(); err != nil {
			log.Fatalf("USB gadget setup: %v", err)
		}
		defer gadget.Teardown()
		log.Printf("USB gadget active, backing: %s", diskPath)
	} else {
		log.Printf("USB gadget skipped (-no-gadget)")
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	log.Println("Shutting down...")
	if gadget != nil {
		gadget.Teardown()
	}
	server.Unmount()
}
