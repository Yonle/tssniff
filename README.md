# tssniff

`tssniff` is a Linux userspace utility that presents a **fake USB storage medium** to a connected host, captures the MPEG-TS data the host writes to it, and makes that data available as an HTTP stream.

The host sees an ordinary MBR-partitioned storage device and writes to it normally. `tssniff` tracks the filesystem metadata needed to identify recording file data. MPEG-TS recording data is broadcast over HTTP instead of being permanently written to the sparse backing image by default.

**FAT32 is the default filesystem. NTFS is also supported.**

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
 ┌──┴───────────────────────────────┐
 │                                  │
 │      filesystem-aware tracker    │
 │        ┌────────┴────────┐       │
 │        │                 │       │
 │      FAT32              NTFS     │
 │     (default)             │       │
 │ metadata window     MFT/extents  │
 │        └────────┬────────┘       │
 │                 ▼                │
 │             TS detector          │
 │                 │                │
 │                 ▼                │
 │            HTTP /stream          │
 │                                  │
 └───────────────┬──────────────────┘
                 │
                 ▼
          sparse backing image
             (guoxin.img)
```

The gadget's LUN points at a file exposed by `tssniff` through FUSE. Every `write()` the host issues against that file lands in `DiskNode.Write`, where the offset is classified according to the selected filesystem:

* For **FAT32**, writes in the filesystem metadata area are persisted. Writes outside that area are treated as recording-data candidates and are broadcast instead of being persisted by default.
* For **NTFS**, metadata is distributed throughout the volume, so physical writes cannot be classified by a single offset boundary. Known file-data extents are treated as recording candidates. A data write that arrives before NTFS metadata identifies its file extent is temporarily persisted to the sparse backing image; once the MFT update makes the extent discoverable, `tssniff` replays those bytes into the TS detector and, with `-preserve` disabled, punches the temporary range back into a hole.

With `-preserve`, recording data is retained in the sparse backing image at its real disk offsets. Without it, the backing image is kept as a fake storage device rather than an archive of the recording stream.

## Why FUSE

The two flags that matter are `DirectMount` and `FOPEN_DIRECT_IO`. Together they keep the host's writes on the FUSE callback path, so `DiskNode.Write` sees each SCSI WRITE without relying on the normal delayed dirty-page writeback path. Both flags are set unconditionally by `mountDiskFS`.

This is also why the USB gadget must back onto the FUSE file directly. Putting a loop device on top of a FUSE file reintroduces the block layer between the host and `tssniff`, and with it the buffering/writeback behaviour that this project is specifically trying to avoid. The gadget's LUN points at `/mnt/tsdisk/disk.img`, not at a loop device.

`DisableSplice` is also set. On some kernels — notably certain SBC BSP forks — the kernel's `splice()` path from a FUSE connection into the backing file can fault pages in a way the backing filesystem cannot satisfy, delivering `SIGBUS` to the daemon. Disabling splice uses ordinary read/write on the FUSE fd instead, at the cost of one extra memory copy per I/O. Metadata writes are small enough that the cost is negligible.

## Requirements

### Kernel

| Requirement                        | Purpose                                     | Check                                                    |
| ---------------------------------- | ------------------------------------------- | -------------------------------------------------------- |
| FUSE                               | Provides `/dev/fuse` and the mount plumbing | `ls /dev/fuse`                                           |
| `CONFIG_USB_CONFIGFS`              | ConfigFS gadget support                     | `zgrep CONFIG_USB_CONFIGFS /proc/config.gz`              |
| `CONFIG_USB_CONFIGFS_MASS_STORAGE` | Mass storage function                       | `zgrep CONFIG_USB_CONFIGFS_MASS_STORAGE /proc/config.gz` |
| `CONFIG_USB_LIBCOMPOSITE`          | Backing for configfs gadgets                | `zgrep CONFIG_USB_LIBCOMPOSITE /proc/config.gz`          |
| A USB Device Controller            | Physical USB peripheral port                | `ls /sys/class/udc/`                                     |

On most SBC images these are already present. On generic distributions they may be modules and load on demand.

### Userspace

| Package      | Provides                     | Needed by                    |
| ------------ | ---------------------------- | ---------------------------- |
| `fuse3`      | `fusermount3`, `libfuse3`    | `tssniff` mount              |
| `util-linux` | `sfdisk`, `losetup`, `blkid` | `gadget.sh prepare-fakedisk` |
| `dosfstools` | `mkfs.fat`                   | FAT32 fake-disk preparation  |
| Go ≥ 1.20    | Building `tssniff`           | `go build`                   |

If `gadget.sh` is configured to create an NTFS test image, install the NTFS formatting utility used by that environment (commonly `mkntfs`, provided by an NTFS userspace package on Debian-family systems).

Install on Debian/Ubuntu/Raspberry Pi OS:

```sh
sudo apt install fuse3 dosfstools
```

For NTFS image preparation, install the distribution's `mkntfs`/NTFS tools package as needed.

## Build

```sh
git clone https://github.com/Yonle/tssniff
cd tssniff
go mod tidy
go build -o tssniff .
```

## Usage

`tssniff` defaults to FAT32:

```sh
sudo ./tssniff \
    -image /srv/guoxin.img \
    -mount /mnt/tsdisk \
    -listen :6969
```

To use NTFS instead:

```sh
sudo ./tssniff \
    -fs ntfs \
    -image /srv/guoxin.img \
    -mount /mnt/tsdisk \
    -listen :6969
```

Options:

| Option       | Default           | Description                                                                                                |
| ------------ | ----------------- | ---------------------------------------------------------------------------------------------------------- |
| `-fs`        | `fat32`           | Filesystem tracker to use: `fat32` (default), `ntfs`, `vfat`, or `exfat` as supported by the tracker layer |
| `-image`     | `/srv/guoxin.img` | Sparse backing image for the fake storage medium                                                           |
| `-mount`     | `/mnt/tsdisk`     | FUSE mount point                                                                                           |
| `-listen`    | `:6969`           | HTTP listen address                                                                                        |
| `-preserve`  | `false`           | Retain MPEG-TS recording data in the sparse image                                                          |
| `-no-gadget` | `false`           | Skip USB gadget setup (for local testing)                                                                  |
| `-debug`     | `false`           | FUSE debug logging                                                                                         |
| `-verbose`   | `false`           | Verbose logging (write classification, TS filtering, tracker state)                                        |

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

## Filesystem handling

### FAT32

FAT32 is the default tracker.

FAT32 has a relatively simple physical layout for this use case. `tssniff` derives a metadata boundary from the boot sector, covering the reserved sectors, both FAT copies, and the root-directory area used by the fake volume.

Writes outside that boundary are treated as recording-data candidates immediately. This makes the FAT32 path low-latency and requires no filesystem allocation tracking before a recording write can be inspected.

FAT32 recorders may also reuse earlier physical disk regions, particularly for timeshift-style rolling storage. The MPEG-TS detector therefore does not assume that every lower physical offset is invalid merely because a later write has already advanced the current stream frontier. Backward writes can be independently probed for a new MPEG-TS segment without blindly splicing them into the existing stream.

### NTFS

NTFS can be selected with:

```sh
-fs ntfs
```

The metadata is distributed throughout the volume, and a recording file's physical clusters may be far away from its logical file offsets.

`tssniff` reads and tracks the MFT to find unnamed nonresident `$DATA` streams belonging to ordinary user files. Each such file is assigned a logical stream ID such as:

```text
ntfs:35
```

Physical file extents carry both a physical disk offset and a logical file-stream offset. That lets a fragmented NTFS file be reconstructed in logical order instead of in physical-disk order.

There is an additional timing problem: an NTFS data extent can be written before the MFT metadata update that tells `tssniff` what file owns that extent. For that reason, an as-yet-unclassified NTFS write is temporarily persisted to the sparse backing image. When the MFT update arrives, `tssniff` discovers the new extent, rereads those bytes, feeds them to the MPEG-TS detector in logical order, and — with `-preserve=false` — punches the temporary physical range back into a sparse hole.

NTFS writes can also move backwards in a file's logical address space. A backward offset is treated as a discontinuity for sequential MPEG-TS emission, not as proof that the bytes are invalid. The TS detector may independently recognize a backward region as a new MPEG-TS segment.

This delayed-discovery path is the main difference between the NTFS and FAT32 trackers.

## `-preserve` semantics

The sparse backing image is the fake storage medium exposed to the host. It is **not** automatically an MPEG-TS archive.

With the default `-preserve=false`:

```text
Host write
    │
    ├── filesystem metadata ───────► sparse image
    │
    └── known recording data ──────► HTTP stream
                                      │
                                      └── not retained
```

For NTFS, an initially unknown write is temporarily stored because the filesystem has not yet told the tracker that the range belongs to a recording:

```text
NTFS data write
    │
    ▼
temporarily store in sparse image
    │
    ▼
MFT update reveals file extent
    │
    ▼
replay bytes to TS detector
    │
    ▼
punch hole when -preserve=false
```

With `-preserve=true`, recording-data ranges are kept in the sparse image after they have been captured. This makes later read-back of the recording possible.

## Disk layout

The fake disk is an MBR image with one filesystem partition. The filesystem can be FAT32 or NTFS depending on `-fs` and how the backing image was prepared:

```text
+---------------------------+
| MBR                       |  sector 0
+---------------------------+
| alignment / free space    |  typically begins at LBA 2048
+---------------------------+
| Partition 1               |
| FAT32 or NTFS             |
+---------------------------+
```

The backing image is sparse. Its logical size is the full disk size, while its allocated size reflects only the parts that `tssniff` has actually materialized.

For FAT32, the normal non-preserve workload primarily materializes the filesystem metadata region.

For NTFS, metadata is distributed across the volume and the image may also temporarily materialize newly written recording extents until the MFT reveals them. Those ranges are punched back into holes after replay when `-preserve=false`. Consequently, the exact allocated size is workload-dependent rather than being determined by one fixed metadata-window size.

Because the image is sparse, `du` and `ls -l` report fundamentally different quantities:

```text
ls -l   → logical file size
 du -h  → allocated storage actually consumed
```

They are not expected to match for a sparse image.

## Putting the image on tmpfs

For the lowest possible metadata write latency, point `-image` at `/dev/shm` (tmpfs):

```sh
cp --sparse=always /srv/guoxin.img /dev/shm/guoxin.img
sudo ./tssniff -image /dev/shm/guoxin.img
```

Sparse copy (`--sparse=always`) preserves the hole layout, so the tmpfs copy initially occupies only the pages that the original image has materialized.

Check the tmpfs size before choosing this path:

```sh
df -h /dev/shm
```

tmpfs commonly defaults to a fraction of physical RAM. On a memory-tight SBC, raise the limit via `/etc/fstab` or, for a temporary change:

```sh
mount -o remount,size=512M /dev/shm
```

If tmpfs runs out of pages mid-write, the kernel can deliver `SIGBUS` to whichever process faults the page. This is not the same failure mode as running out of space on a normal block device.

The image on tmpfs does not survive reboot. If you need persistence across reboots, keep a copy on disk and copy it in at startup.

## Storage behaviour

Three different quantities are involved, and they should not be confused:

| Quantity                      | Reported by              | Meaning                                                        |
| ----------------------------- | ------------------------ | -------------------------------------------------------------- |
| Fake-disk logical size        | `ls -l`, `stat`          | What the host believes the storage medium contains             |
| Filesystem-visible allocation | Host filesystem tools    | What the host believes it has allocated inside the fake volume |
| Backing image allocation      | `du` on the sparse image | Physical storage actually materialized for `guoxin.img`        |

For a `.ts` recording with `-preserve=false`, the recording's file-data blocks are normally not retained permanently in the backing image. On NTFS, they may exist there transiently while waiting for MFT classification.

For `-preserve=true`, recording extents remain materialized in the backing image at their real physical offsets, so the sparse image can grow substantially with the amount of recorded data.

## TS extraction

A filesystem write is not automatically an MPEG-TS stream. Recorders may perform bookkeeping updates inside recording files, and NTFS may expose multiple ordinary files whose `$DATA` streams are all candidates.

The TS detector therefore works on logical file-stream order and uses packet-level validation.

### Stream continuity

A candidate stream has a logical byte frontier.

A write exactly at the frontier continues the current sequential stream.

A write ahead of the frontier indicates a missing range, so the detector resets its sequential buffer rather than blindly stitching unrelated bytes together.

A write below the frontier is treated as a **backward segment**. It is not appended to the current sequential buffer, but the bytes may be accumulated separately and probed for a strong MPEG-TS signature. This allows timeshift/ring-buffer style reuse of an earlier region without causing ordinary backward filesystem updates to corrupt the active stream.

This rule applies to both FAT32 and NTFS. The meaning of the offset differs between them:

* FAT32 currently uses the physical disk offset.
* NTFS uses the logical file-stream offset.

### Stream selection

NTFS can contain several candidate files at once. Once one logical stream has been positively identified as MPEG-TS, unrelated candidate streams do not get to splice themselves into the active broadcast.

A genuinely new recording can replace the active stream. The NTFS tracker signals this using file-stream discovery/reuse state rather than assuming that the first physical extent of the file must be logical offset zero.

### Directory reset

A write to the root directory metadata can reset the active TS detector. This gives a fresh recording a clean state even when the recorder creates a new file whose physical location is unrelated to the previous recording.

### MPEG-TS validation

Accepted bytes are buffered until complete 188-byte transport-stream packets are available. Detection requires consecutive packet positions beginning with the MPEG-TS sync byte `0x47` and basic header sanity checks. Partial packets at the end of a filesystem write are kept until subsequent bytes arrive.

Backward segments are probed using the same packet-level validation before they are promoted to a new sequential TS segment.

This prevents ordinary filesystem bookkeeping from being sent directly to HTTP clients.

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

The backing image path is controlled by `BACKING_IMAGE`.

## Testing without a USB gadget

The full path can be exercised locally without a physical USB port:

```sh
sudo ./tssniff -verbose -no-gadget
```

The filesystem used by the test image must match the tracker selected with `-fs`.

For example, with a FAT32 image:

```sh
sudo mount -o loop,offset=1048576,sync /mnt/tsdisk/disk.img /mnt/guoxin
```

For NTFS, mount the NTFS test image with the host's normal NTFS filesystem support rather than assuming the FAT32 command above is sufficient.

Writes to filesystem metadata are persisted to the sparse image. Recording-data writes are broadcast and, unless `-preserve` is enabled, are not retained permanently.

`gadget.sh` also provides this workflow:

```sh
sudo ./gadget.sh prepare-tssniff
sudo ./gadget.sh mount
sudo ./gadget.sh unmount
sudo ./gadget.sh stop-tssniff
```

Do not create a loop device over `/mnt/tsdisk/disk.img` while `tssniff` is running with the gadget active. The loop driver and the USB gadget cannot both safely own the file without corrupting the classification. Use a second image, or stop the gadget first.

## USB gadget

The gadget exposes a single Mass Storage function:

```text
USB Mass Storage
└── LUN 0
    └── /mnt/tsdisk/disk.img
```

The kernel presents this to the host as a normal removable USB drive. The host performs its own MBR and filesystem parsing; `tssniff` does not mount that filesystem itself. It observes the resulting storage writes through FUSE and uses the selected tracker to classify them.

## HTTP server

`tssniff` runs an HTTP server on the address given by `-listen`.

```text
GET /stream
```

The response is a chunked `video/mp2t` stream. Headers are flushed immediately on connect. The body is the sequence of transport-stream bytes the host has written and that the TS detector has accepted.

The hub runs on its own goroutine. `Hub.Broadcast` is a non-blocking enqueue: the FUSE write path never waits for an HTTP client. A slow client only fills its own queue; other clients are unaffected. When a queue is full, the oldest queued chunk for that client is dropped to keep the live stream moving.

The HTTP server is independent of the USB gadget. The gadget provides the storage interface to the host; the HTTP server provides the extracted stream to network clients.

## Kernel stability notes

On some ARM64 SBC kernels — notably the MSM8916 mainline and its derivatives — recording sessions can trigger an **RCU stall** that freezes the whole system, including the USB gadget and the FUSE daemon. The symptom is that the STB suddenly stops seeing the storage device and `dmesg` fills with messages such as:

```text
rcu: INFO: rcu_preempt detected stalls on CPUs/tasks:
rcu: rcu_preempt kthread starved for 9319 jiffies!
rcu: Unless rcu_preempt kthread gets sufficient CPU time, OOM is now expected behavior.
```

The stack trace of the stuck CPU may point at `tick_check_broadcast_expired` inside `cpu_idle_poll`. This is a kernel idle/timer problem, not an MPEG-TS parser problem.

## Status

`tssniff` is experimental software for presenting a fake USB storage medium and capturing MPEG-TS writes on Linux.

**Supported filesystem trackers:**

* **FAT32** — default
* **NTFS** — optional via `-fs ntfs`

The FAT32 tracker uses filesystem geometry from the boot sector and classifies data by the derived metadata boundary. The NTFS tracker reconstructs unnamed nonresident `$DATA` streams from MFT information and handles delayed discovery of newly allocated recording extents.

The TS detector treats backward writes as potentially meaningful recording segments rather than automatically discarding them. This accommodates timeshift/ring-buffer behaviour where recording data can reappear at lower physical or logical offsets.

The host's filesystem driver may report the volume as "not properly unmounted" after a recording session because `tssniff` does not emulate every shutdown-time filesystem transaction exactly. Treat the fake disk as a capture mechanism rather than a general-purpose storage volume.

The TS extraction rules assume a single recording file is being written at a time and that a recording stream can be reconstructed in logical order from the observed filesystem writes. A host that interleaves several active recordings, rewrites large portions of an already-recorded stream, or relies on filesystem features outside the tracker's supported subset may not be captured correctly.
