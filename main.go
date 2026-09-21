package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
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

	pendingMu sync.Mutex
	pending   []PendingWrite

	replayMu sync.Mutex

	quarantine     *os.File
	quarantineNext int64

	hwmMu sync.Mutex
	hwm   map[string]uint64
}

type PendingWrite struct {
	Offset      uint64
	SpoolOffset int64
	Length      int
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

var (
	verbLog bool
)

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
	return fs.NewListDirStream([]fuse.DirEntry{
		{
			Name: "disk.img",
			Ino:  2,
			Mode: syscall.S_IFREG,
		},
	}), 0
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

func (d *DiskNode) reserveTSRange(
	name string,
	start uint64,
	data []byte,
) (uint64, []byte) {
	if len(data) == 0 || name == "" {
		return start, nil
	}

	d.hwmMu.Lock()
	defer d.hwmMu.Unlock()

	mark := d.hwm[name]

	if start < mark {
		skip := mark - start

		if skip >= uint64(len(data)) {
			return 0, nil
		}

		start += skip
		data = data[skip:]
	}

	d.hwm[name] = start + uint64(len(data))

	return start, data
}

func (d *DiskNode) Open(
	ctx context.Context,
	flags uint32,
) (fs.FileHandle, uint32, syscall.Errno) {
	/*
		VERY IMPORTANT:

		Do not implement FilePassthroughFder.

		If go-fuse gives the kernel our real FD as a passthrough
		handle, the kernel can perform I/O directly against the
		backing file and our Read/Write callbacks disappear.

		FOPEN_DIRECT_IO keeps file I/O going through our FUSE server.
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

	/*
		Base layer.
	*/
	n, err := d.fd.ReadAt(
		data,
		off,
	)

	if err != nil && !errors.Is(err, io.EOF) {
		return fuse.ReadResultData(nil), toErrno(err)
	}

	/*
		Sparse holes return zeroes naturally, but enforce the
		virtual disk semantics if ReadAt returns short.
	*/
	for i := n; i < len(data); i++ {
		data[i] = 0
	}

	/*
		Known TS data was never persisted.
		Make the backing image appear to contain zeroes there.
	*/
	for _, r := range d.tracker.Snapshot() {
		if r.Kind != RangeTS {
			continue
		}

		a := maxU64(start, r.Start)
		b := minU64(start+length, r.End)

		if a >= b {
			continue
		}

		clear(data[int(a-start):int(b-start)])
	}

	/*
		Unknown writes live in the quarantine overlay.
		Later writes win.
	*/
	for _, p := range d.pendingSnapshot() {
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

		n, err := d.quarantine.ReadAt(
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

	if start > d.size {
		return 0, syscall.EFBIG
	}

	if uint64(len(data)) > d.size-start {
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
			newStart, newData := d.reserveTSRange(
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
					newStart,
					len(newData),
					seg.Name,
				)
			}

			/*
				BroadcastBatch owns the data before returning,
				so slices into the FUSE request are safe here.
			*/
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
			if err := d.queuePending(
				seg.Start,
				part,
			); err != nil {
				return 0, toErrno(err)
			}
		}
	}

	if len(tsBatch) > 0 {
		d.hub.BroadcastBatch(tsBatch)
	}

	/*
		The filesystem metadata write may tell us that previously
		unknown data is actually a .ts file.

		Refresh AFTER writing the metadata to the backing image.
	*/
	if metadataChanged {
		log.Printf(
			"filesystem metadata changed; refreshing tracker",
		)

		if err := d.tracker.Refresh(); err != nil {
			log.Printf(
				"tracker refresh: %v",
				err,
			)

			/*
				Keep the write successful. The STB must not see
				our internal classification failure.
			*/
		} else {
			if err := d.resolvePending(); err != nil {
				log.Printf(
					"pending resolution: %v",
					err,
				)
			}
		}
	}

	return uint32(len(data)), 0
}

func (d *DiskNode) Flush(
	ctx context.Context,
	fh fs.FileHandle,
) syscall.Errno {
	if err := d.fd.Sync(); err != nil {
		return toErrno(err)
	}

	return 0
}

func (d *DiskNode) Fsync(
	ctx context.Context,
	fh fs.FileHandle,
	flags uint32,
) syscall.Errno {
	if err := d.fd.Sync(); err != nil {
		return toErrno(err)
	}

	return 0
}

func toErrno(err error) syscall.Errno {
	if err == nil {
		return 0
	}

	var errno syscall.Errno

	if errors.As(err, &errno) {
		return errno
	}

	return syscall.EIO
}

type Hub struct {
	mu      sync.Mutex
	clients map[*Client]struct{}
}

type Client struct {
	conn net.Conn
	q    chan []byte
}

func NewHub() *Hub {
	return &Hub{
		clients: make(map[*Client]struct{}),
	}
}

func (h *Hub) Serve(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	log.Printf(
		"TS TCP server listening on %s",
		addr,
	)

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}

		c := &Client{
			conn: conn,
			q:    make(chan []byte, 256),
		}

		h.mu.Lock()
		h.clients[c] = struct{}{}
		count := len(h.clients)
		h.mu.Unlock()

		log.Printf(
			"TS client connected: %s (%d clients)",
			conn.RemoteAddr(),
			count,
		)

		go func() {
			defer func() {
				/*
					Anything still queued belongs to this client and is
					no longer going to be written.
				*/
				for payload := range c.q {
					releasePayload(payload)
				}

				_ = conn.Close()

				h.mu.Lock()
				delete(h.clients, c)
				count := len(h.clients)
				h.mu.Unlock()

				log.Printf(
					"TS client disconnected: %s (%d clients)",
					conn.RemoteAddr(),
					count,
				)
			}()

			for payload := range c.q {
				err := writeFull(
					conn,
					payload,
				)

				releasePayload(payload)

				if err != nil {
					return
				}
			}
		}()
	}
}

func writeFull(
	w net.Conn,
	p []byte,
) error {
	offset := 0

	for offset < len(p) {
		n, err := w.Write(p[offset:])
		if err != nil {
			return err
		}

		if n == 0 {
			return io.ErrShortWrite
		}

		offset += n
	}

	return nil
}

func (h *Hub) BroadcastBatch(parts [][]byte) {
	if len(parts) == 0 {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	for c := range h.clients {
		dropped := false

		for _, data := range parts {
			payload := acquirePayload(len(data))
			copy(payload, data)

			select {
			case c.q <- payload:

			default:
				releasePayload(payload)
				dropped = true
			}
		}

		if dropped {
			_ = c.conn.Close()
			close(c.q)
			delete(h.clients, c)

			log.Printf("dropped slow TS client")
		}
	}
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
		"TCP TS listener",
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

	root := &DiskFS{}

	disk := &DiskNode{
		fd:         fd,
		size:       uint64(st.Size()),
		tracker:    tracker,
		hub:        hub,
		quarantine: quarantine,
		hwm:        make(map[string]uint64),
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

				/*
					USB storage I/O normally lands here in
					manageable chunks.
				*/
				MaxWrite: 128 * 1024,

				Debug: *debug,
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
				"TCP server stopped: %v",
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
		_ = server.Unmount()
	}()

	server.Wait()
}

func (d *DiskNode) queuePending(
	offset uint64,
	data []byte,
) error {
	if len(data) == 0 {
		return nil
	}

	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()

	if d.quarantine == nil {
		return errors.New(
			"quarantine file is not open",
		)
	}

	spoolOffset := d.quarantineNext

	n, err := d.quarantine.WriteAt(
		data,
		spoolOffset,
	)
	if err != nil {
		return err
	}

	if n != len(data) {
		return io.ErrShortWrite
	}

	d.pending = append(
		d.pending,
		PendingWrite{
			Offset:      offset,
			SpoolOffset: spoolOffset,
			Length:      n,
		},
	)

	d.quarantineNext += int64(n)

	if verbLog {
		log.Printf(
			"QUARANTINE offset=%d size=%d",
			offset,
			n,
		)
	}

	return nil
}

func (d *DiskNode) pendingSnapshot() []PendingWrite {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()

	if len(d.pending) == 0 {
		return nil
	}

	out := make(
		[]PendingWrite,
		len(d.pending),
	)

	copy(
		out,
		d.pending,
	)

	return out
}

func (d *DiskNode) resolvePending() error {
	d.replayMu.Lock()
	defer d.replayMu.Unlock()

	d.pendingMu.Lock()

	pendingLen := len(d.pending)

	pending := make(
		[]PendingWrite,
		pendingLen,
	)

	copy(
		pending,
		d.pending,
	)

	d.pendingMu.Unlock()

	if len(pending) == 0 {
		return nil
	}

	stillPending := make(
		[]PendingWrite,
		0,
		len(pending),
	)

	var buf []byte

	var tsBatch [][]byte

	for _, p := range pending {
		if cap(buf) < p.Length {
			buf = make([]byte, p.Length)
		} else {
			buf = buf[:p.Length]
		}

		n, err := d.quarantine.ReadAt(
			buf,
			p.SpoolOffset,
		)

		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}

		if n != p.Length {
			return io.ErrUnexpectedEOF
		}

		segments := d.tracker.Classify(
			p.Offset,
			uint64(p.Length),
		)

		tsBatch = tsBatch[:0]

		for _, seg := range segments {
			rel := seg.Start - p.Offset
			length := seg.End - seg.Start

			part := buf[int(rel):int(rel+length)]

			switch seg.Kind {
			case RangeTS:
				newStart, newData := d.reserveTSRange(
					seg.Name,
					seg.Start,
					part,
				)

				if len(newData) == 0 {
					continue
				}

				if verbLog {
					log.Printf(
						"REPLAY TS offset=%d size=%d file=%s",
						newStart,
						len(newData),
						seg.Name,
					)
				}

				/*
					BroadcastBatch copies the payload before returning,
					so this slice can point directly into buf.
				*/
				tsBatch = append(
					tsBatch,
					newData,
				)

			case RangeNormal, RangeMeta:
				nn, err := d.fd.WriteAt(
					part,
					int64(seg.Start),
				)

				if err != nil {
					return err
				}

				if nn != len(part) {
					return io.ErrShortWrite
				}

			case RangeUnknown:
				stillPending = append(
					stillPending,
					PendingWrite{
						Offset: seg.Start,
						SpoolOffset: p.SpoolOffset +
							int64(rel),
						Length: int(length),
					},
				)
			}
		}

		if len(tsBatch) > 0 {
			d.hub.BroadcastBatch(tsBatch)
		}
	}

	d.pendingMu.Lock()

	/*
		Preserve anything that was added while replay was running.
		Those writes are newer than the pending snapshot we just
		processed.
	*/
	if len(d.pending) > pendingLen {
		stillPending = append(
			stillPending,
			d.pending[pendingLen:]...,
		)
	}

	d.pending = stillPending

	/*
		Only reset the spool when nothing remains.
	*/
	if len(d.pending) == 0 {
		if err := d.quarantine.Truncate(0); err != nil {
			d.pendingMu.Unlock()
			return err
		}

		d.quarantineNext = 0
	}

	d.pendingMu.Unlock()

	return nil
}