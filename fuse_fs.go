package main

import (
	"context"
	"os"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type DiskFS struct {
	fs.Inode
}

var _ fs.NodeGetattrer = (*DiskFS)(nil)

func (r *DiskFS) Getattr(
	ctx context.Context,
	fh fs.FileHandle,
	out *fuse.AttrOut,
) syscall.Errno {
	out.Mode = syscall.S_IFDIR | 0755
	return 0
}

type DiskFSOpts struct {
	MountPoint string
	Image      *os.File
	Hub        *Hub
	Tracker    *FSTracker
	Preserve   bool
	Shm        *ShmBuffer
	Debug      bool
}

func mountDiskFS(o DiskFSOpts) (*fuse.Server, error) {
	st, err := o.Image.Stat()
	if err != nil {
		return nil, err
	}

	root := &DiskFS{}

	return fs.Mount(o.MountPoint, root, &fs.Options{
		MountOptions: fuse.MountOptions{
			AllowOther:    true,
			DirectMount:   true,
			MaxWrite:      188 * 697,
			Debug:         o.Debug,
			DisableSplice: true,
		},

		OnAdd: func(ctx context.Context) {
			node := &DiskNode{
				imgFile:  o.Image,
				size:     uint64(st.Size()),
				hub:      o.Hub,
				tracker:  o.Tracker,
				preserve: o.Preserve,
				shm:      o.Shm,
			}

			child := root.NewPersistentInode(
				ctx,
				node,
				fs.StableAttr{
					Mode: syscall.S_IFREG,
					Ino:  2,
				},
			)

			root.AddChild("disk.img", child, true)
		},
	})
}
