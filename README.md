# tssniff

`tssniff` is a Linux userspace utility that presents a **fake USB storage medium** to a connected host, detects MPEG-TS data written to it, and exposes the captured data as an HTTP stream.

The host sees an ordinary MBR-partitioned NTFS USB drive. `tssniff` exposes the disk through FUSE, persists writes asynchronously, tracks NTFS file-data ranges, and punches captured recording data back to sparse holes.

## How it works

```text
Host / STB
    │  USB Mass Storage
    ▼
USB gadget
    │  /mnt/tsdisk/disk.img
    ▼
tssniff (FUSE)
    │
    ├── SHM write staging
    │
    ├── Sniffer
    │     └── MPEG-TS detector
    │
    ├── Writer
    │     └── backing image
    │
    ├── NTFS tracker
    │     └── MFT / file-data ranges
    │
    ├── Reconciler
    │     └── punchout
    │
    └── HTTP /stream
```

The USB gadget's Mass Storage LUN points to a file exposed by `tssniff` through FUSE.

Host writes are staged in shared memory and submitted to an asynchronous pipeline. The sniffer examines the writes for MPEG-TS data while the writer persists them to the backing image.

The NTFS tracker reads the Master File Table (MFT) to determine which physical ranges belong to ordinary files. Captured MPEG-TS ranges are reconciled against those ranges and punched back to sparse holes after they are identified.

## Requirements

Kernel:

* FUSE (`/dev/fuse`)
* `CONFIG_USB_CONFIGFS`
* `CONFIG_USB_CONFIGFS_MASS_STORAGE`
* `CONFIG_USB_LIBCOMPOSITE`
* a USB Device Controller (`/sys/class/udc/`)

Userspace:

* `fuse3`
* `util-linux`
* `ntfs-3g` / `mkntfs`
* Go 1.20 or newer

Debian-family systems:

```sh
sudo apt install fuse3 util-linux ntfs-3g
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
    -mount /mnt/tsdisk \
    -listen :6969
```

| Option       | Default           | Description               |
| ------------ | ----------------- | ------------------------- |
| `-image`     | `/srv/guoxin.img` | NTFS backing image        |
| `-mount`     | `/mnt/tsdisk`     | FUSE mount point          |
| `-listen`    | `:6969`           | HTTP listen address       |
| `-no-gadget` | `false`           | Skip USB gadget setup     |
| `-debug`     | `false`           | Enable FUSE debug logging |
| `-verbose`   | `false`           | Enable verbose logging    |

Stream clients:

```sh
mpv http://127.0.0.1:6969/stream
ffmpeg -i http://127.0.0.1:6969/stream -c copy out.ts
```

## NTFS handling

`tssniff` uses the NTFS Master File Table to track physical file-data ownership.

It tracks ordinary in-use files with unnamed, nonresident `$DATA` streams and converts their NTFS runlists into physical byte ranges.

A recording file can be physically fragmented. Its data therefore does not have to occupy one contiguous region of the backing image.

MFT updates may arrive separately from file-data writes. Captured TS ranges are kept in a fixed journal so they can be matched later when the corresponding NTFS data range becomes known.

The tracker only handles physical ownership. MPEG-TS detection is performed independently by the sniffer.

## MPEG-TS extraction

The sniffer operates directly on host writes.

A write is checked for MPEG-TS packet structure, including:

* 188-byte packet alignment
* `0x47` sync bytes
* basic MPEG-TS header validation
* consecutive valid packets

Once MPEG-TS is detected, complete packets are emitted to `/stream`.

Physical fragmentation is handled by treating non-contiguous writes as separate candidates rather than blindly concatenating unrelated filesystem writes.

The FUSE `Write()` path only stages the incoming data and submits it to the asynchronous pipeline. MPEG-TS detection, HTTP delivery, NTFS parsing, and punchout do not run directly in the FUSE write handler.

## Disk layout

The fake disk uses an MBR partition table with one NTFS partition.

```text
+---------------------------+
| MBR                       | sector 0
+---------------------------+
| alignment                 | typically LBA 2048
+---------------------------+
| Partition 1 (NTFS)        |
+---------------------------+
```

The backing image is sparse.

```sh
ls -lh /srv/guoxin.img
du -h /srv/guoxin.img
```

`ls` reports the logical file size. `du` reports allocated storage.

The image can therefore have a logical size of 1 TB while using much less physical storage.

## Backing image on tmpfs

For low-latency temporary storage, the backing image can be placed on `/dev/shm`:

```sh
cp --sparse=always /srv/guoxin.img /dev/shm/guoxin.img

sudo ./tssniff \
    -image /dev/shm/guoxin.img
```

Check available space first:

```sh
df -h /dev/shm
```

The image is temporary and does not survive a reboot. Running out of tmpfs space can result in `SIGBUS`.

## `gadget.sh`

`gadget.sh` is a small lifecycle wrapper around `tssniff`.

It only handles:

```text
prepare-fakedisk    Create the sparse NTFS image if missing
start               Prepare the image and start tssniff
stop                Stop tssniff
```

Examples:

```sh
sudo ./gadget.sh prepare-fakedisk
sudo ./gadget.sh start
sudo ./gadget.sh stop
```

The wrapper prepares the NTFS image, loads `libcomposite` and ConfigFS when starting, then launches `tssniff`.

`tssniff` itself owns the USB gadget configuration and teardown.

## Testing without a USB gadget

Run:

```sh
sudo ./tssniff -verbose -no-gadget
```

The FUSE-exposed disk can then be mounted locally:

```sh
sudo mount \
    -t ntfs \
    -o loop,offset=1048576,sync \
    /mnt/tsdisk/disk.img \
    /mnt/guoxin
```

Do not create a separate loop device over `/mnt/tsdisk/disk.img` while the USB gadget is active.

## USB gadget

`tssniff` creates a single USB Mass Storage function with LUN 0 pointing at:

```text
/mnt/tsdisk/disk.img
```

The host performs its own MBR and NTFS parsing. `tssniff` does not mount the NTFS filesystem itself.

## HTTP server

```text
GET /stream
```

returns a chunked `video/mp2t` stream.

Clients may connect before recording starts and remain connected while waiting for MPEG-TS data.

The HTTP hub uses buffered queues, so a slow client does not block the FUSE write path.

## Punchout

`tssniff` punches identified recording-data ranges back to sparse holes using `FALLOC_FL_PUNCH_HOLE`.

The backing filesystem must support sparse-file hole punching. Common Linux filesystems that support it include ext4, XFS, and Btrfs.

The NTFS filesystem inside the fake disk is unrelated to the filesystem containing the backing image.

Punchout works from the physical ranges discovered through the NTFS MFT. This allows recording data to be captured and then released from the backing image while leaving the logical disk size unchanged.

## Status

Experimental.

`tssniff` is designed around recording workloads that write MPEG-TS data to an NTFS-formatted USB storage device. It is not intended to emulate every aspect of a general-purpose USB disk or every NTFS filesystem transaction.

The extraction logic currently assumes that the recording workload can be identified from the host writes and NTFS metadata observed by `tssniff`.
