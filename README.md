# tssniff

`tssniff` is a Linux userspace utility that presents a **fake USB storage medium** to a connected host, captures the MPEG-TS data the host writes to it, and makes that data available as an HTTP stream.

The host sees an ordinary MBR-partitioned FAT32 (or exFAT) disk and writes to it normally. The filesystem metadata is persisted to a backing image; the file data of MPEG-TS recordings is broadcast to HTTP clients instead of being written to disk.

## Architecture

```text
Host / STB
    │
    │ USB Mass Storage
    ▼
 USB gadget (configfs)              kernel
    │
    │ /dev/nbd0  (kernel NBD client)
    ▼
 tssniff                            userspace
 ┌──┴──────────────┐
 │                 │
metadata writes    file data writes
 │                 │
 ▼                 ▼
sparse image       HTTP /stream
(guoxin.img)
```

The host issues sector-level reads and writes to a block device. The kernel NBD driver forwards those requests to `tssniff` over a Unix socket. `tssniff` classifies each write by offset:

- Writes inside the filesystem metadata window (MBR, boot sector, both FAT tables, root directory) are persisted to the sparse backing image.
- Writes outside that window are treated as file data and broadcast over HTTP. They are not written to the sparse image unless `-preserve` is set, in which case they are written to `/dev/shm`.

The metadata window is derived from the FAT boot sector at startup, so it scales with the size of the filesystem. A 1 TB FAT32 volume with 64 KB clusters has a metadata window of about 134 MiB.

## Why NBD

The kernel's block layer requires block-level I/O semantics (sector alignment, atomic writes, read-after-write consistency). A FUSE filesystem cannot provide these reliably when a loop device is placed on top of it. NBD presents a real block device to the kernel, which is what a USB Mass Storage gadget needs.

## Requirements

### Kernel

| Requirement | Purpose | Check |
|---|---|---|
| Linux ≥ 4.10 | Newstyle NBD handshake | `uname -r` |
| `CONFIG_BLK_DEV_NBD` | Provides `/dev/nbd*` | `zgrep CONFIG_BLK_DEV_NBD /proc/config.gz` |
| `CONFIG_USB_CONFIGFS` | ConfigFS gadget support | `zgrep CONFIG_USB_CONFIGFS /proc/config.gz` |
| `CONFIG_USB_CONFIGFS_MASS_STORAGE` | Mass storage function | `zgrep CONFIG_USB_CONFIGFS_MASS_STORAGE /proc/config.gz` |
| `CONFIG_USB_LIBCOMPOSITE` | Backing for configfs gadgets | `zgrep CONFIG_USB_LIBCOMPOSITE /proc/config.gz` |
| A USB Device Controller | Physical USB peripheral port | `ls /sys/class/udc/` |

On most SBC images these are already present. On generic distributions they are usually modules and load on demand.

### Userspace

| Package | Provides | Needed by |
|---|---|---|
| `nbd-client` | `/usr/sbin/nbd-client` | `tssniff` (attaches `/dev/nbd0`) |
| `util-linux` | `sfdisk`, `losetup`, `blkid` | `gadget.sh prepare-fakedisk` |
| `dosfstools` | `mkfs.fat` | FAT32 formatting |
| `exfatprogs` | `mkfs.exfat` | exFAT formatting (only if `FILESYSTEM=exfat`) |
| Go ≥ 1.20 | Building `tssniff` | `go build` |

Install on Debian/Ubuntu/Raspberry Pi OS:

```sh
sudo apt install nbd-client dosfstools exfatprogs
```

## Build

```sh
git clone https://github.com/Yonle/tssniff
cd tssniff
go mod tidy
go build -o tssniff .
```

## Usage

```sh
sudo ./tssniff \
    -image /srv/guoxin.img \
    -nbd /dev/nbd0 \
    -listen :6969 \
    -fs fat32
```

Options:

| Option | Default | Description |
|---|---|---|
| `-image` | `/srv/guoxin.img` | Sparse backing image for filesystem metadata |
| `-nbd` | `/dev/nbd0` | Kernel NBD device to attach to |
| `-listen` | `:6969` | HTTP listen address |
| `-fs` | `fat32` | Filesystem type (`fat32` or `exfat`) |
| `-preserve` | `false` | Also write file data to `/dev/shm` (never to the sparse image) |
| `-no-gadget` | `false` | Skip USB gadget setup (for local testing) |
| `-verbose` | `false` | Verbose logging |

The intercepted MPEG-TS stream is available at:

```text
http://<host>:6969/stream
```

Clients:

```sh
mpv http://localhost:6969/stream
ffmpeg -i http://127.0.0.1:6969/stream -c copy out.ts
```

A slow HTTP client is disconnected rather than blocking storage I/O. There is no authentication; run the HTTP server on a trusted network.

## Disk layout

The fake disk is a standard MBR image with one FAT32 (or exFAT) partition:

```text
+---------------------------+
| MBR                       |  sector 0
+---------------------------+
| alignment                 |  LBA 2048
+---------------------------+
| Partition 1               |
| FAT32 (or exFAT)          |
+---------------------------+
```

The backing image is sparse. Its logical size is the full disk size, but its allocated size on disk is only what the metadata window costs. For a 1 TB FAT32 volume, this is roughly 130–150 MiB after a full recording session, regardless of how many gigabytes of MPEG-TS were written.

## Storage behaviour

Three different quantities are involved, and they should not be confused:

| Quantity | Reported by | Meaning |
|---|---|---|
| File logical size | `ls -l`, `stat` | What the host believes it wrote |
| File allocated size | `du` on the mounted filesystem | FAT cluster chain × cluster size |
| Backing image size | `du` on the sparse image | Actual bytes on the host disk |

For a `.ts` recording, the first two reflect what the host sees. The third is unaffected by the size of the recording. Reading a recorded file back from the host side returns zeros; the host itself never reads back a recording while it is being written, so this is not visible to it.

## `gadget.sh`

`gadget.sh` prepares the fake disk, starts `tssniff`, and configures the USB Mass Storage gadget.

Commands:

```text
prepare-fakedisk    Create the sparse backing image if missing
prepare-tssniff     modprobe nbd, start tssniff, wait for /dev/nbd0
stop-tssniff        Detach NBD and stop tssniff
mount               TEST ONLY: mount the exported filesystem locally
unmount             TEST ONLY: unmount it
prepare-gadget      Create the configfs USB gadget (does not bind UDC)
find-udc            Print available UDCs
start               prepare-fakedisk + prepare-tssniff + prepare-gadget + bind
stop                Unbind gadget, remove it, stop tssniff
status              Show current state of everything
```

Typical usage:

```sh
sudo ./gadget.sh start
sudo ./gadget.sh status
sudo ./gadget.sh stop
```

## Testing without a USB gadget

The NBD path can be exercised locally without a physical USB port:

```sh
sudo modprobe nbd nbds_max=4 max_part=16
sudo ./tssniff -verbose -no-gadget
```

Then, from another shell:

```sh
sudo blockdev --getsize64 /dev/nbd0
sudo fdisk -l /dev/nbd0
sudo mount /dev/nbd0p1 /mnt/guoxin
```

Writes to the metadata region are persisted. Writes to file data are broadcast and can be observed at `http://localhost:6969/stream`.

`gadget.sh` also provides this workflow:

```sh
sudo ./gadget.sh prepare-tssniff
sudo ./gadget.sh mount
sudo ./gadget.sh unmount
sudo ./gadget.sh stop-tssniff
```

## USB gadget

The gadget exposes a single Mass Storage function:

```text
USB Mass Storage
└── LUN 0
    └── /dev/nbd0
```

The kernel presents this to the host as a normal removable USB drive. The host performs its own MBR parsing and filesystem mounting; `tssniff` does not interpret the host's filesystem structure beyond identifying the metadata window.

## HTTP server

`tssniff` runs an HTTP server on the address given by `-listen`.

```text
GET /stream
```

The response is a chunked `video/mp2t` stream. Its body is the sequence of MPEG-TS payload bytes the host has written, in the order they were received by `tssniff`.

The HTTP server is independent of the USB gadget. The gadget provides the storage interface to the host; the HTTP server provides the same data to network clients.

## Status

`tssniff` is experimental software for presenting a fake USB storage medium and capturing MPEG-TS writes on Linux.

FAT32 is the default filesystem and is the only one currently supported end-to-end. exFAT can be selected for the fake disk, but the metadata window for exFAT falls back to a fixed 4 MiB region and may misclassify writes on large volumes. Treat exFAT as untested.

The host's filesystem driver may report the volume as "not properly unmounted" after a recording session, because `tssniff` does not intercept shutdown-time writes from a host that has already stopped writing. This is cosmetic; the filesystem remains consistent.