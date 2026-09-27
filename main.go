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
	mountPoint := flag.String(
		"mount",
		"/mnt/tsdisk",
		"FUSE mount point",
	)

	image := flag.String(
		"image",
		"/srv/guoxin.img",
		"sparse backing image",
	)

	listenAddr := flag.String(
		"listen",
		":6969",
		"HTTP stream listener",
	)

	filesystem := flag.String(
		"fs",
		"ntfs",
		"filesystem type (supported: ntfs, fat32)",
	)

	debug := flag.Bool(
		"debug",
		false,
		"FUSE debug",
	)

	preserve := flag.Bool(
		"preserve",
		false,
		"preserve TS to sparse disk",
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
		*image,
		os.O_RDWR,
		0666,
	)
	if err != nil {
		log.Fatalf(
			"open sparse image %s: %v",
			*image,
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
			*image,
		)
	}

	size := uint64(st.Size())

	part, err := findMBRPartition(
		imgFile,
		*filesystem,
	)
	if err != nil {
		log.Printf(
			"Warning: MBR partition check: %v (raw device mode)",
			err,
		)
	} else if verbLog {
		log.Printf(
			"Found %s partition at offset %d, size %d",
			*filesystem,
			part.Offset,
			part.Size,
		)
	}

	/*
		SHM is now the authoritative volatile disk overlay.

		It lives in /dev/shm rather than in a Go []byte cache.
	*/
	shm, err := NewSHMDisk(
		imgFile,
		size,
	)
	if err != nil {
		log.Fatalf(
			"initialize SHM overlay: %v",
			err,
		)
	}

	if verbLog {
		log.Printf(
			"SHM overlay: %s",
			shm.path,
		)
	}

	/*
		The tracker MUST read through SHM.

		That means NTFS sees acknowledged-but-not-yet-persisted
		writes exactly like FUSE Read() does.
	*/
	var tracker Tracker

	switch *filesystem {
	case "ntfs":
		tracker, err = NewNTFSTracker(
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

	case "fat32", "vfat", "exfat":
		tracker = NewFSTracker(
			*filesystem,
			part,
			shm,
		)

	default:
		_ = shm.Close()

		log.Fatalf(
			"unsupported filesystem %q",
			*filesystem,
		)
	}

	if verbLog {
		log.Printf(
			"tracker: metaEnd=%d",
			tracker.MetadataEnd(),
		)
	}

	/*
		The writer owns ALL slow access to the real sparse image.
	*/
	writer := NewDiskWriter(
		imgFile,
		shm,
	)

	if err := os.MkdirAll(
		*mountPoint,
		0755,
	); err != nil {
		_ = writer.Close()
		_ = shm.Close()

		log.Fatalf(
			"mkdir %s: %v",
			*mountPoint,
			err,
		)
	}

	hub := NewHub()

	go startStreamServer(
		*listenAddr,
		hub,
	)

	server, err := mountDiskFS(
		DiskFSOpts{
			MountPoint: *mountPoint,
			Image:      imgFile,
			SHM:        shm,
			Writer:     writer,
			Hub:        hub,
			Tracker:    tracker,
			Preserve:   *preserve,
			Debug:      *debug,
		},
	)
	if err != nil {
		_ = writer.Close()
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

	log.Printf(
		"Stream:    http://localhost%s/stream",
		*listenAddr,
	)

	var gadget *USBGadget

	if !*noGadget {
		gadget = NewUSBGadget(
			diskPath,
		)

		if err := gadget.Setup(); err != nil {
			server.Unmount()
			_ = writer.Close()
			_ = shm.Close()

			log.Fatalf(
				"USB gadget setup: %v",
				err,
			)
		}

		log.Printf(
			"USB gadget active, backing: %s",
			diskPath,
		)
	} else {
		log.Printf(
			"USB gadget skipped (-no-gadget)",
		)
	}

	c := make(chan os.Signal, 1)

	signal.Notify(
		c,
		os.Interrupt,
		syscall.SIGTERM,
	)

	<-c

	log.Println(
		"Shutting down...",
	)

	/*
		Stop the USB consumer first.
	*/
	if gadget != nil {
		gadget.Teardown()
	}

	/*
		Stop accepting new FUSE requests.
	*/
	server.Unmount()

	/*
		Now drain every already-accepted physical operation.

		This is the ONLY place where shutdown waits for the real disk.
	*/
	if err := writer.Close(); err != nil {
		log.Printf(
			"physical writer shutdown: %v",
			err,
		)
	}

	/*
		Only after the writer is completely gone do we remove /dev/shm.
	*/
	if err := shm.Close(); err != nil {
		log.Printf(
			"SHM shutdown: %v",
			err,
		)
	}
}
