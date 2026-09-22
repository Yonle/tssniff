# tssniff

`tssniff` is a Linux userspace utility that presents a **fake USB storage medium** to a connected host, captures the MPEG-TS data the host writes to it, and makes that data available as an HTTP stream.

The host sees an ordinary MBR-partitioned FAT32 (or exFAT) disk and writes to it normally. Filesystem metadata is persisted to a sparse backing image. File data of MPEG-TS recordings is broadcast to HTTP clients instead of being written to disk.

## Architecture

```text
Host / STB
    │
    │ USB Mass Storage
    ▼
 USB gadget (configfs)              kernel
    │
    │ /mnt/tsdisk/disk.img  (regular file)
    ▼
 tssniff (FUSE)                     userspace
 ┌──┴──────────────┐
 │                 │
metadata writes    file data writes
 │                 │
 ▼                 ▼
sparse image       HTTP /stream
(guoxin.img)
```

The gadget's LUN points at a file exposed by `tssniff` through FUSE. Every `write()` the host issues against that file lands in `DiskNode.Write`, where the offset is classified:

- Writes inside the filesystem metadata window (MBR, boot sector, both FAT tables, root directory) are persisted to the sparse backing image.
- Writes outside that window are treated as recording data. They are broadcast over HTTP and are **not** written to the sparse image. With `-preserve`, they are additionally written to `/dev/shm` (tmpfs), never to the sparse image.

The metadata window is derived from the FAT boot sector at startup, so it scales with the size of the filesystem. A 1 TB FAT32 volume with 64 KB clusters has a metadata window of roughly 134 MiB.

## Why FUSE

The two flags that matter are `DirectMount` and `FOPEN_DIRECT_IO`. Together they keep the host's writes on the FUSE callback path, so `DiskNode.Write` sees each SCSI WRITE within microseconds. Without them, the kernel page cache holds dirty pages for up to `dirty_expire_centisecs` (30 s by default), which makes the HTTP stream lag by tens of seconds and breaks the illusion of a live recorder. Both flags are set unconditionally by `mountDiskFS`.

This is also why the USB gadget must back onto the FUSE file directly. Putting a loop device on top of a FUSE file reintroduces the block layer between the host and `tssniff`, and with it the same 30-second writeback delay. The gadget's LUN points at `/mnt/tsdisk/disk.img`, not at a loop device.

## Requirements

### Kernel

| Requirement | Purpose | Check |
|---|---|---|
| FUSE | Provides `/dev/fuse` and the mount plumbing | `ls /dev/fuse` |
| `CONFIG_USB_CONFIGFS` | ConfigFS gadget support | `zgrep CONFIG_USB_CONFIGFS /proc/config.gz` |
| `CONFIG_USB_CONFIGFS_MASS_STORAGE` | Mass storage function | `zgrep CONFIG_USB_CONFIGFS_MASS_STORAGE /proc/config.gz` |
| `CONFIG_USB_LIBCOMPOSITE` | Backing for configfs gadgets | `zgrep CONFIG_USB_LIBCOMPOSITE /proc/config.gz` |
| A USB Device Controller | Physical USB peripheral port | `ls /sys/class/udc/` |

On most SBC images these are already present. On generic distributions they are usually modules and load on demand.

### Userspace

| Package | Provides | Needed by |
|---|---|---|
| `fuse3` | `fusermount3`, `libfuse3` | `tssniff` mount |
| `util-linux` | `sfdisk`, `losetup`, `blkid` | `gadget.sh prepare-fakedisk` |
| `dosfstools` | `mkfs.fat` | FAT32 formatting |
| `exfatprogs` | `mkfs.exfat` | exFAT formatting (only if `FILESYSTEM=exfat`) |
| Go ≥ 1.20 | Building `tssniff` | `go build` |

Install on Debian/Ubuntu/Raspberry Pi OS:

```sh
sudo apt install fuse3 dosfstools exfatprogs
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
    -listen :6969 \
    -fs fat32
```

Options:

| Option | Default | Description |
|---|---|---|
| `-image` | `/srv/guoxin.img` | Sparse backing image for filesystem metadata |
| `-mount` | `/mnt/tsdisk` | FUSE mount point |
| `-listen` | `:6969` | HTTP listen address |
| `-fs` | `fat32` | Filesystem type (`fat32` or `exfat`) |
| `-preserve` | `false` | Also write recording data to `/dev/shm` |
| `-no-gadget` | `false` | Skip USB gadget setup (for local testing) |
| `-debug` | `false` | FUSE debug logging |
| `-verbose` | `false` | Verbose logging (write classification, TS filtering, tracker state) |

The captured stream is available at:

```text
http://<host>:6969/stream
```

Clients:

```sh
mpv http://localhost:6969/stream
ffmpeg -i http://127.0.0.1:6969/stream -c copy out.ts
```

Response headers are flushed as soon as a client connects, before any data is available. A client that opens the stream while nothing is recording sees a live connection waiting for data, not a hang.

A client that cannot keep up is not disconnected. The hub drops the oldest queued chunk for that client to make room for the newest one. This keeps the live stream flowing and avoids the reconnect stutter that a disconnect-on-slow policy causes. There is no authentication; run the HTTP server on a trusted network.

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

## TS extraction

Not everything above the metadata window is transport stream. A DVB recorder writes small bookkeeping blocks (file headers, index updates) at fixed offsets inside the recording file. Those writes are above the metadata window and would be broadcast verbatim if classification were purely offset-based.

`DiskNode.broadcastTS` keeps only valid transport stream bytes, using three rules:

1. **Directional continuity.** A TS write is accepted only if its offset is at or above the frontier of the current stream (the byte after the last accepted TS write).
   - Writes **below** the frontier are bookkeeping at the file's start. They are dropped silently.
   - Writes **at** the frontier are normal continuation.
   - Writes **above** the frontier are the first write of a new recording. The pending buffer is flushed, and the frontier jumps to the new offset.

2. **Directory reset.** A write to the root directory cluster (where the recorder creates or finalises a `.ts` entry) resets the frontier. This ensures the next recording starts with a clean state even if the recorder picks a starting offset below the previous frontier.

3. **Stride resync.** Accepted bytes are appended to a small buffer, and only complete runs of 188-byte packets beginning with `0x47` are broadcast. A partial packet at the end of the buffer is retained until the next write completes it.

Together these mean the HTTP stream contains only valid, contiguous transport stream packets. Bookkeeping writes, FAT table updates, and directory entry churn never reach clients.

## `gadget.sh`

`gadget.sh` prepares the fake disk, starts `tssniff`, and configures the USB Mass Storage gadget.

Commands:

```text
prepare-fakedisk    Create the sparse backing image if missing
prepare-tssniff     Start tssniff in the background, wait for the FUSE file
stop-tssniff        Stop the tssniff instance started by this script
mount               TEST ONLY: mount the FUSE file locally
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

The full path can be exercised locally without a physical USB port:

```sh
sudo ./tssniff -verbose -no-gadget
```

Then, from another shell:

```sh
sudo mount -o loop,offset=1048576,sync /mnt/tsdisk/disk.img /mnt/guoxin
```

Writes to the metadata region persist to the sparse image. Writes to file data are broadcast and can be observed at `http://localhost:6969/stream`.

`gadget.sh` also provides this workflow:

```sh
sudo ./gadget.sh prepare-tssniff
sudo ./gadget.sh mount
sudo ./gadget.sh unmount
sudo ./gadget.sh stop-tssniff
```

Do not create a loop device over `/mnt/tsdisk/disk.img` while `tssniff` is running with the gadget active. The loop driver and the USB gadget cannot both own the file without corrupting the classification. Use a second image, or stop the gadget first.

## USB gadget

The gadget exposes a single Mass Storage function:

```text
USB Mass Storage
└── LUN 0
    └── /mnt/tsdisk/disk.img
```

The kernel presents this to the host as a normal removable USB drive. The host performs its own MBR parsing and filesystem mounting; `tssniff` does not interpret the host's filesystem structure beyond identifying the metadata window.

## HTTP server

`tssniff` runs an HTTP server on the address given by `-listen`.

```text
GET /stream
```

The response is a chunked `video/mp2t` stream. Headers are flushed immediately on connect. The body is the sequence of transport stream bytes the host has written, filtered by the rules described in *TS extraction* above.

The HTTP server is independent of the USB gadget. The gadget provides the storage interface to the host; the HTTP server provides the same data to network clients.

## Status

`tssniff` is experimental software for presenting a fake USB storage medium and capturing MPEG-TS writes on Linux.

FAT32 is the default filesystem and is the only one currently supported end-to-end. exFAT can be selected for the fake disk, but the metadata window for exFAT falls back to a fixed 4 MiB region and may misclassify writes on large volumes. Treat exFAT as untested.

The host's filesystem driver may report the volume as "not properly unmounted" after a recording session, because `tssniff` does not intercept shutdown-time writes from a host that has already stopped writing. This is cosmetic; the filesystem remains consistent.

The TS extraction rules assume a single recording file is being written at a time, that its data clusters are written in monotonically increasing order, and that the host only writes backwards when updating the file's bookkeeping region. A host that interleaves two concurrent recordings, or that seeks backwards to rewrite recorded data, will not be captured correctly. The directional continuity check drops anything that does not continue the current stream rather than risk corrupting it; the directory reset ensures a fresh recording always starts cleanly regardless of where its first cluster sits on the disk.