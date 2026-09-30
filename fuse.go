package main

import (
	"context"
	"errors"
	"log"
	"os"
	"sync/atomic"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type DiskFS struct {
	fs.Inode
}

var _ fs.NodeGetattrer = (*DiskFS)(nil)

func (d *DiskFS) Getattr(
	ctx context.Context,
	fh fs.FileHandle,
	out *fuse.AttrOut,
) syscall.Errno {
	out.Mode =
		syscall.S_IFDIR |
			0o755

	return 0
}

type DiskNode struct {
	fs.Inode

	imgFile *os.File

	size atomic.Uint64
	seq  atomic.Uint64

	shm *SHMDisk

	writer *Writer

	pipeline *WritePipeline

	ntfs *NTFS
}

var (
	_ fs.NodeGetattrer = (*DiskNode)(nil)
	_ fs.NodeOpener    = (*DiskNode)(nil)
	_ fs.NodeReader    = (*DiskNode)(nil)
	_ fs.NodeWriter    = (*DiskNode)(nil)
	_ fs.NodeFlusher   = (*DiskNode)(nil)
	_ fs.NodeFsyncer   = (*DiskNode)(nil)
)

func (d *DiskNode) Getattr(
	ctx context.Context,
	fh fs.FileHandle,
	out *fuse.AttrOut,
) syscall.Errno {
	size := d.size.Load()

	out.Mode =
		syscall.S_IFREG |
			0o644

	out.Size = size
	out.Blksize = 512
	out.Blocks =
		(size + 511) /
			512

	return 0
}

func (d *DiskNode) Open(
	ctx context.Context,
	flags uint32,
) (fs.FileHandle, uint32, syscall.Errno) {
	/*
		Keep the current direct-I/O mode.

		We deliberately do not request parallel direct writes.
	*/
	return nil,
		fuse.FOPEN_DIRECT_IO,
		0
}

func (d *DiskNode) Read(
	ctx context.Context,
	fh fs.FileHandle,
	dest []byte,
	off int64,
) (fuse.ReadResult, syscall.Errno) {
	if off < 0 {
		return fuse.ReadResultData(nil),
			syscall.EINVAL
	}

	size := d.size.Load()

	start := uint64(off)

	if start >= size ||
		len(dest) == 0 {
		return fuse.ReadResultData(nil),
			0
	}

	length := uint64(len(dest))

	if length > size-start {
		length =
			size - start
	}

	data := dest[:int(length)]

	if _, err := d.shm.ReadAt(
		data,
		off,
	); err != nil {
		return fuse.ReadResultData(nil),
			toErrno(err)
	}

	if verbLog {
		log.Printf(
			"read off=%d len=%d",
			start,
			length,
		)
	}

	return fuse.ReadResultData(data), 0
}

func (d *DiskNode) Write(
	ctx context.Context,
	fh fs.FileHandle,
	data []byte,
	off int64,
) (n uint32, errno syscall.Errno) {
	/*
		Nothing downstream is allowed to turn this request into
		a long synchronous operation.
	*/
	defer func() {
		if r := recover(); r != nil {
			log.Printf(
				"panic in Write off=%d len=%d: %v",
				off,
				len(data),
				r,
			)

			n = 0
			errno = syscall.EIO
		}
	}()

	if off < 0 {
		return 0,
			syscall.EINVAL
	}

	if len(data) == 0 {
		return 0, 0
	}

	start := uint64(off)

	if uint64(len(data)) >
		^uint64(0)-start {
		return 0,
			syscall.EFBIG
	}

	end :=
		start +
			uint64(len(data))

	/*
		SHM is the only synchronous storage operation.

		The STB must see its bytes immediately through FUSE Read().
	*/
	staged, err := d.shm.StageWrite(
		data,
		start,
	)
	if err != nil {
		log.Printf(
			"SHM stage failed off=%d len=%d: %v",
			start,
			len(data),
			err,
		)

		return 0,
			syscall.EIO
	}

	if end >
		d.size.Load() {
		d.size.Store(end)
	}

	seq := d.seq.Add(1)

	/*
		One immutable copy is shared by the writer and sniffer.

		The FUSE-provided buffer cannot be retained after this method
		returns.
	*/
	owned := append(
		[]byte(nil),
		data...,
	)

	touchesMFT := d.ntfs.TouchesMFT(
		start,
		end,
	)

	ev := WriteEvent{
		Seq: seq,

		Offset: start,
		Data:   owned,

		Staged: staged,

		TouchesMFT: touchesMFT,
	}

	/*
		Crucially:

		    SubmitWrite() is non-blocking.

		If the pipeline is full, another goroutine performs the channel
		send instead. This FUSE goroutine never waits for it.
	*/
	if err := d.pipeline.SubmitWrite(
		ev,
	); err != nil {
		log.Printf(
			"write pipeline rejected seq=%d: %v",
			seq,
			err,
		)

		return 0, toErrno(err)
	}

	return uint32(len(data)), 0
}

func (d *DiskNode) Flush(
	ctx context.Context,
	fh fs.FileHandle,
) syscall.Errno {
	return d.syncWriter(ctx)
}

func (d *DiskNode) Fsync(
	ctx context.Context,
	fh fs.FileHandle,
	flags uint32,
) syscall.Errno {
	return d.syncWriter(ctx)
}

func (d *DiskNode) syncWriter(
	ctx context.Context,
) syscall.Errno {
	target := d.seq.Load()

	done := make(chan error, 1)

	go func() {
		err := d.pipeline.Sync(target)

		done <- err
	}()

	select {
	case err := <-done:
		return toErrno(err)

	case <-ctx.Done():
		return syscall.EINTR
	}
}

func mountDiskFS(
	mountPoint string,
	image *os.File,
	shm *SHMDisk,
	writer *Writer,
	pipeline *WritePipeline,
	ntfs *NTFS,
	debug bool,
) (*fuse.Server, *DiskNode, error) {
	st, err := image.Stat()
	if err != nil {
		return nil,
			nil,
			err
	}

	root := &DiskFS{}

	node := &DiskNode{
		imgFile: image,

		shm: shm,

		writer: writer,

		pipeline: pipeline,

		ntfs: ntfs,
	}

	node.size.Store(
		uint64(st.Size()),
	)

	server, err := fs.Mount(
		mountPoint,
		root,
		&fs.Options{
			MountOptions: fuse.MountOptions{
				AllowOther: true,

				DirectMount: true,

				MaxWrite: 188 * 697,

				Debug: debug,

				DisableSplice: true,
			},

			OnAdd: func(
				ctx context.Context,
			) {
				child := root.NewPersistentInode(
					ctx,
					node,
					fs.StableAttr{
						Mode: syscall.S_IFREG,

						Ino: 2,
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
		return nil,
			nil,
			err
	}

	return server,
		node,
		nil
}

func toErrno(
	err error,
) syscall.Errno {
	if err == nil {
		return 0
	}

	var errno syscall.Errno

	if errors.As(
		err,
		&errno,
	) {
		return errno
	}

	return syscall.EIO
}
