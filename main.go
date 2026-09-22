package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type DiskFS struct {
	fs.Inode
}

var (
	_ fs.InodeEmbedder = (*DiskFS)(nil)
	_ fs.NodeGetattrer = (*DiskFS)(nil)
	_ fs.NodeReaddirer = (*DiskFS)(nil)
)

type DiskNode struct {
	fs.Inode

	fd      *os.File
	size    uint64
	tracker Tracker
	hub     *Hub
	overlay *Overlay
}

var (
	_ fs.InodeEmbedder = (*DiskNode)(nil)
	_ fs.NodeGetattrer = (*DiskNode)(nil)
	_ fs.NodeOpener    = (*DiskNode)(nil)
	_ fs.NodeReader    = (*DiskNode)(nil)
	_ fs.NodeWriter    = (*DiskNode)(nil)
	_ fs.NodeFlusher   = (*DiskNode)(nil)
	_ fs.NodeFsyncer   = (*DiskNode)(nil)
)

var verbLog bool

func (r *DiskFS) Getattr(
	ctx context.Context,
	fh fs.FileHandle,
	out *fuse.AttrOut,
) syscall.Errno {
	out.Mode = syscall.S_IFDIR | 0755
	return 0
}

func (r *DiskFS) Readdir(
	ctx context.Context,
) (fs.DirStream, syscall.Errno) {
	return fs.NewListDirStream([]fuse.DirEntry{{
		Name: "disk.img",
		Ino:  2,
		Mode: syscall.S_IFREG,
	}}), 0
}

func (d *DiskNode) Getattr(
	ctx context.Context,
	fh fs.FileHandle,
	out *fuse.AttrOut,
) syscall.Errno {
	out.Mode = syscall.S_IFREG | 0644
	out.Size = d.size
	out.Blksize = 512
	out.Blocks = d.size / 512
	return 0
}

func (d *DiskNode) Open(
	ctx context.Context,
	flags uint32,
) (fs.FileHandle, uint32, syscall.Errno) {
	/*
		Keep I/O in our FUSE callbacks. Passthrough would let the
		kernel bypass the virtualized read/write path.
	*/
	return nil, fuse.FOPEN_DIRECT_IO, 0
}

func (d *DiskNode) Read(
	ctx context.Context,
	fh fs.FileHandle,
	dest []byte,
	off int64,
) (fuse.ReadResult, syscall.Errno) {
	if off < 0 {
		return fuse.ReadResultData(nil), syscall.EINVAL
	}

	start := uint64(off)

	if start >= d.size || len(dest) == 0 {
		return fuse.ReadResultData(nil), 0
	}

	length := uint64(len(dest))

	if length > d.size-start {
		length = d.size - start
	}

	data := dest[:int(length)]

	n, err := d.fd.ReadAt(data, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return fuse.ReadResultData(nil), toErrno(err)
	}

	if n < len(data) {
		clear(data[n:])
	}

	/*
		Known TS data was never persisted.
		Make the backing image appear to contain zeroes there.
	*/
	for _, r := range d.tracker.TSRanges() {
		if r.End <= start || r.Start >= start+length {
			continue
		}

		a := maxU64(start, r.Start)
		b := minU64(start+length, r.End)

		clear(data[int(a-start):int(b-start)])
	}

	/*
		Unknown writes live in the overlay.
		Later writes win.
	*/
	pending := d.overlay.BeginRead()
	defer d.overlay.EndRead()

	for _, p := range pending {
		a := maxU64(start, p.Offset)
		b := minU64(
			start+length,
			p.Offset+uint64(p.Length),
		)

		if a >= b {
			continue
		}

		srcStart := a - p.Offset
		dstStart := a - start
		size := int(b - a)

		n, err := d.overlay.quarantine.ReadAt(
			data[int(dstStart):int(dstStart)+size],
			p.SpoolOffset+int64(srcStart),
		)
		if err != nil && !errors.Is(err, io.EOF) {
			return fuse.ReadResultData(nil), toErrno(err)
		}

		if n != size {
			return fuse.ReadResultData(nil), syscall.EIO
		}
	}

	return fuse.ReadResultData(data), 0
}

func (d *DiskNode) Write(
	ctx context.Context,
	fh fs.FileHandle,
	data []byte,
	off int64,
) (uint32, syscall.Errno) {
	if off < 0 {
		return 0, syscall.EINVAL
	}

	start := uint64(off)

	if start > d.size ||
		uint64(len(data)) > d.size-start {
		return 0, syscall.EFBIG
	}

	if len(data) == 0 {
		return 0, 0
	}

	segments := d.tracker.Classify(
		start,
		uint64(len(data)),
	)

	metadataChanged := false
	var tsBatch [][]byte

	for _, seg := range segments {
		rel := seg.Start - start
		length := seg.End - seg.Start

		part := data[rel : rel+length]

		switch seg.Kind {
		case RangeTS:
			_, newData := d.overlay.ReserveTSRange(
				seg.Name,
				seg.Start,
				part,
			)

			if len(newData) == 0 {
				continue
			}

			if verbLog {
				log.Printf(
					"TS WRITE offset=%d size=%d file=%s",
					seg.Start,
					len(newData),
					seg.Name,
				)
			}

			tsBatch = append(
				tsBatch,
				newData,
			)

		case RangeNormal, RangeMeta:
			n, err := d.fd.WriteAt(
				part,
				int64(seg.Start),
			)

			if err != nil {
				return uint32(rel + uint64(n)), toErrno(err)
			}

			if n != len(part) {
				return uint32(rel + uint64(n)), syscall.EIO
			}

			if seg.Kind == RangeMeta {
				metadataChanged = true
			}

		case RangeUnknown:
			_, ok := findMPEGTSOffset(part)

			if ok {
				if len(part) != 0 {
					if verbLog {
						log.Printf(
							"TS FALLBACK offset=%d size=%d",
							seg.Start,
							len(part),
						)
					}

					tsBatch = append(
						tsBatch,
						part,
					)
				}

				continue
			}

			/*
				Everything else remains speculative in the overlay.
			*/
			if err := d.overlay.Queue(
				seg.Start,
				part,
			); err != nil {
				return 0, toErrno(err)
			}
		}
	}

	if len(tsBatch) != 0 {
		d.hub.BroadcastBatch(tsBatch)
	}

	/*
		Metadata is committed first, then the tracker is refreshed
		so a newly-created recording can become a known TS range.
	*/
	if metadataChanged {
		if err := d.overlay.RefreshAndResolve(); err != nil {
			log.Printf(
				"tracker refresh/resolution: %v",
				err,
			)
		}
	}

	return uint32(len(data)), 0
}

func (d *DiskNode) Flush(
	ctx context.Context,
	fh fs.FileHandle,
) syscall.Errno {
	return toErrno(d.fd.Sync())
}

func (d *DiskNode) Fsync(
	ctx context.Context,
	fh fs.FileHandle,
	flags uint32,
) syscall.Errno {
	return toErrno(d.fd.Sync())
}

func newTracker(
	fd *os.File,
	partition Partition,
	filesystem string,
) (Tracker, error) {
	switch filesystem {
	case "fat32", "vfat":
		return NewFAT32Tracker(
			fd,
			partition,
		)

	case "exfat":
		return NewExfatTracker(
			fd,
			partition,
		), nil

	default:
		return nil, fmt.Errorf(
			"unsupported filesystem %q",
			filesystem,
		)
	}
}

func main() {
	mountPoint := flag.String(
		"mount",
		"/mnt/tsdisk",
		"FUSE mount point",
	)

	image := flag.String(
		"image",
		"/srv/guoxin.img",
		"real sparse backing image",
	)

	listenAddr := flag.String(
		"listen",
		":6969",
		"HTTP TS listener",
	)

	filesystem := flag.String(
		"fs",
		"fat32",
		"filesystem to use",
	)

	debug := flag.Bool(
		"debug",
		false,
		"FUSE debug",
	)

	flag.BoolVar(
		&verbLog,
		"verbose",
		false,
		"Be more verbose",
	)

	flag.Parse()

	st, err := os.Stat(*image)
	if err != nil {
		log.Fatal(err)
	}

	if !st.Mode().IsRegular() {
		log.Fatalf(
			"%s is not a regular file",
			*image,
		)
	}

	if st.Size() <= 0 {
		log.Fatalf(
			"%s is empty",
			*image,
		)
	}

	fd, err := os.OpenFile(
		*image,
		os.O_RDWR,
		0,
	)
	if err != nil {
		log.Fatal(err)
	}
	defer fd.Close()

	quarantinePath := filepath.Join(
		"/dev/shm",
		filepath.Base(*image)+".quarantine",
	)

	quarantine, err := os.OpenFile(
		quarantinePath,
		os.O_RDWR|os.O_CREATE|os.O_TRUNC,
		0600,
	)
	if err != nil {
		log.Fatal(
			"open quarantine file: ",
			err,
		)
	}

	defer quarantine.Close()
	defer os.Remove(quarantinePath)

	hub := NewHub()

	partition, err := findMBRPartition(
		fd,
		*filesystem,
	)
	if err != nil {
		log.Fatal(
			"find partition: ",
			err,
		)
	}

	log.Printf(
		"%s partition: offset=%d size=%d type=0x%02x",
		*filesystem,
		partition.Offset,
		partition.Size,
		partition.Type,
	)

	tracker, err := newTracker(
		fd,
		partition,
		*filesystem,
	)
	if err != nil {
		log.Fatal(
			"create tracker: ",
			err,
		)
	}

	if err := tracker.Refresh(); err != nil {
		log.Fatal(
			"initial tracker scan: ",
			err,
		)
	}

	overlay := NewOverlay(
		fd,
		quarantine,
		tracker,
		hub,
	)
	defer overlay.Close()

	root := &DiskFS{}

	disk := &DiskNode{
		fd:      fd,
		size:    uint64(st.Size()),
		tracker: tracker,
		hub:     hub,
		overlay: overlay,
	}

	if err := os.MkdirAll(
		*mountPoint,
		0755,
	); err != nil {
		log.Fatal(err)
	}

	server, err := fs.Mount(
		*mountPoint,
		root,
		&fs.Options{
			MountOptions: fuse.MountOptions{
				AllowOther:  true,
				DirectMount: true,
				MaxWrite:    188 * 697,
				Debug:       *debug,
			},

			OnAdd: func(ctx context.Context) {
				child := root.NewPersistentInode(
					ctx,
					disk,
					fs.StableAttr{
						Mode: syscall.S_IFREG,
						Ino:  2,
					},
				)

				root.AddChild(
					"disk.img",
					child,
					true,
				)
			},
		},
	)
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		if err := hub.Serve(*listenAddr); err != nil {
			log.Printf(
				"HTTP server stopped: %v",
				err,
			)
		}
	}()

	log.Printf(
		"FUSE disk: %s",
		filepath.Join(
			*mountPoint,
			"disk.img",
		),
	)

	log.Printf(
		"logical size: %.2f GiB",
		float64(st.Size())/
			(1024*1024*1024),
	)

	stopCtx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer stop()

	go func() {
		<-stopCtx.Done()

		hub.Close()
		_ = server.Unmount()
	}()

	server.Wait()
}
