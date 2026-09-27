# tssniff

`tssniff` is a Linux userspace utility that presents a **fake USB storage medium** to a connected host, captures the MPEG-TS data the host writes to it, and makes that data available as an HTTP stream.

The host sees an ordinary MBR-partitioned USB drive. Recording data is broadcast over HTTP instead of being written to the backing image.

## How it works

```text
Host / STB
    │  USB Mass Storage
    ▼
USB gadget (configfs)
    │  /mnt/tsdisk/disk.img
    ▼
tssniff (FUSE)
    │
    ├── filesystem tracker
    │       ├── NTFS   (default)
    │       └── FAT32  (supported)
    │
    ├── TS detector
    │
    └── HTTP /stream
    │
    ▼
sparse backing image
```

The gadget's LUN points at a file exposed by `tssniff` through FUSE. Every write the host issues lands in `DiskNode.Write`, where the offset is classified by the selected tracker.

* **NTFS** — the tracker reads the MFT to find unnamed nonresident `$DATA` streams of ordinary user files. A data write that arrives before the MFT identifies its file extent is temporarily stored, replayed into the TS detector once the extent is known, and punched back to a hole.
* **FAT32** — the tracker derives a metadata boundary from the boot sector. Writes outside that boundary are treated as recording candidates.

The captured stream is available at `http://<host>:6969/stream`.

## Requirements

Kernel:

- FUSE (`/dev/fuse`)
- `CONFIG_USB_CONFIGFS`
- `CONFIG_USB_CONFIGFS_MASS_STORAGE`
- `CONFIG_USB_LIBCOMPOSITE`
- a UDC (`/sys/class/udc/`)

Userspace:

- `fuse3`
- `util-linux`
- `dosfstools` (FAT32 only)
- `ntfs-3g` or `mkntfs` (NTFS only)
- Go ≥ 1.20

Debian family:

```sh
sudo apt install fuse3 dosfstools ntfs-3g
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

NTFS is the default. To use FAT32:

```sh
sudo ./tssniff -fs fat32 -image /srv/guoxin.img
```

| Option       | Default           | Description                                  |
| ------------ | ----------------- | -------------------------------------------- |
| `-fs`        | `ntfs`            | Filesystem tracker: `ntfs` or `fat32`        |
| `-image`     | `/srv/guoxin.img` | Sparse backing image                         |
| `-mount`     | `/mnt/tsdisk`     | FUSE mount point                             |
| `-listen`    | `:6969`           | HTTP listen address                          |
| `-preserve`  | `false`           | Retain recording data in the sparse image    |
| `-no-gadget` | `false`           | Skip USB gadget setup                        |
| `-debug`     | `false`           | FUSE debug logging                           |
| `-verbose`   | `false`           | Verbose logging                              |

Clients:

```sh
mpv http://localhost:6969/stream
ffmpeg -i http://127.0.0.1:6969/stream -c copy out.ts
```

## Filesystem handling

### NTFS

Metadata is distributed throughout the volume. `tssniff` reads and tracks the MFT to find unnamed nonresident `$DATA` streams. Each file gets a stream ID such as `ntfs:35`. Extents carry both a physical offset and a logical file offset, so a fragmented file is reconstructed in logical order.

An extent can be written before the MFT entry that identifies it. Such writes are temporarily stored, then replayed into the TS detector when the MFT update arrives, then punched back to a hole (unless `-preserve`).

Backward writes in a file's logical address space are treated as a new segment, not as corruption.

### FAT32

A metadata boundary is derived from the boot sector, covering reserved sectors, both FAT copies, and the root directory. Writes past the boundary are recording candidates.

FAT32 recorders may reuse earlier physical regions (timeshift, ring buffer). Backward writes are probed independently for a new TS segment.

FAT32 does not distinguish directory clusters from file data clusters at the block layer. The tracker classifies by offset only. It is functional but not the focus of this project.

## `-preserve`

The sparse image is the fake storage medium, not automatically an archive.

With `-preserve=false` (default), recording data is broadcast over HTTP and not retained.

With `-preserve=true`, recording extents remain materialized in the image at their real offsets.

On NTFS, newly written extents are transiently materialized until the MFT reveals them, then punched back when `-preserve=false`.

## Disk layout

MBR-partitioned, one filesystem partition:

```text
+---------------------------+
| MBR                       |  sector 0
+---------------------------+
| alignment                 |  typically LBA 2048
+---------------------------+
| Partition 1 (NTFS/FAT32)  |
+---------------------------+
```

The image is sparse. `ls -l` reports logical size; `du` reports allocated size. They are not expected to match.

## tmpfs

For lowest metadata latency, point `-image` at `/dev/shm`:

```sh
cp --sparse=always /srv/guoxin.img /dev/shm/guoxin.img
sudo ./tssniff -image /dev/shm/guoxin.img
```

Check `df -h /dev/shm` first. Running out of tmpfs pages delivers `SIGBUS`, not `ENOSPC`. The image does not survive reboot.

## `gadget.sh`

```text
prepare-fakedisk    Create the sparse backing image if missing
prepare-tssniff     Start tssniff, wait for the FUSE file
stop-tssniff        Stop the tssniff instance from this script
mount / unmount     TEST ONLY: local mount of the FUSE file
prepare-gadget      Create the configfs gadget (does not bind UDC)
find-udc            Print available UDCs
start / stop        Full lifecycle
status              Current state
```

```sh
sudo ./gadget.sh start
sudo ./gadget.sh status
sudo ./gadget.sh stop
```

## Testing without a USB gadget

```sh
sudo ./tssniff -verbose -no-gadget
```

Mount the FUSE file locally with the matching filesystem. For FAT32:

```sh
sudo mount -o loop,offset=1048576,sync /mnt/tsdisk/disk.img /mnt/guoxin
```

Do not create a loop device over `/mnt/tsdisk/disk.img` while the gadget is active. The loop driver and the gadget cannot both own the file safely.

## USB gadget

Single Mass Storage function, LUN 0 points at `/mnt/tsdisk/disk.img`. The host performs its own MBR and filesystem parsing; `tssniff` does not mount that filesystem itself.

## HTTP server

`GET /stream` returns a chunked `video/mp2t` stream. Headers flush immediately on connect. Clients that connect before recording starts see a live connection waiting for data.

The hub runs on its own goroutine. `Broadcast` never blocks the FUSE write path. A slow client only fills its own queue.

## TS extraction

A write is not automatically TS. The detector works on logical file-stream order.

* A write exactly at the frontier continues the stream.
* A write ahead of the frontier resets the sequential buffer.
* A write below the frontier is treated as a backward segment and probed independently for a new TS segment.

NTFS uses logical file offsets. FAT32 uses physical offsets.

Once a logical stream is identified as MPEG-TS, unrelated candidates are not spliced into the active broadcast. A new recording can replace the active stream. Root directory writes can reset the detector.

Accepted bytes are buffered until complete 188-byte packets are available. Detection requires consecutive packets with sync byte `0x47` and header sanity. Partial packets are kept for the next write.

## Status

Experimental. Treat the fake disk as a capture mechanism, not general-purpose storage. The host may report the volume as "not properly unmounted" after a session because `tssniff` does not emulate every shutdown-time filesystem transaction.

The extraction rules assume a single recording file is being written at a time.

### Punchout

When a range is identified as recording data and not preserved, `tssniff` punches it back to a sparse hole in the backing image via `FALLOC_FL_PUNCH_HOLE`. This is why the image stays small for a long recording.

Punchout requires the backing image's host filesystem to support sparse files. **ext4, XFS, and Btrfs support it. FAT32 and exFAT do not.** If you place the backing image on a filesystem without hole-punch support, the punch silently becomes a no-op or an error, and the image grows with the recording.

For NTFS, punchout is applied to extents that the MFT has classified as recording data. For FAT32, the classification is offset-based and punchout is not applied — the image grows 4–8 MB per session as metadata is persisted.
