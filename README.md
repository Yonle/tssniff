# tssniff

`tssniff` is a Linux userspace utility that **fakes being a USB storage medium** using FUSE and a USB Mass Storage gadget.

It presents a normal disk to the connected host while monitoring filesystem writes and intercepting MPEG-TS (`.ts`) file data.

## Architecture

```text
Host / STB
    │
    │ USB Mass Storage
    ▼
  Fake disk
    │
    ▼
  tssniff
 ┌──┴──────────────┐
 │                 │
normal writes    *.ts writes
 │                 │
 ▼                 ▼
storage        interception
                    │
                    ▼
                 TCP :6969
```

The fake disk is a complete disk image containing an MBR partition table and an exFAT partition.

## Usage

```sh
tssniff \
    -image /srv/guoxin.img \
    -mount /mnt/tsdisk \
    -listen :6969
```

Options:

| Option    | Description           |
| --------- | --------------------- |
| `-image`  | Backing disk image    |
| `-mount`  | FUSE mount point      |
| `-listen` | TCP listen address    |
| `-debug`  | Enable FUSE debugging |

## Disk layout

```text
+---------------------------+
| MBR                       |
+---------------------------+
| alignment                 |
+---------------------------+
| Partition 1               |
| exFAT                     |
+---------------------------+
```

`tssniff` reads the MBR to locate the exFAT partition.

Partition detection is separated from filesystem handling so additional partition-table formats can be implemented later.

## Storage handling

The backing image is not directly exposed to the host.

Instead:

```text
backing disk image
        │
        ▼
     tssniff
        │
        ▼
   FUSE disk.img
```

The complete disk image, including the MBR and partition table, is exposed through the FUSE filesystem.

This allows filesystem writes to be inspected before they reach the backing image.

## TS interception

`tssniff` tracks the exFAT filesystem and associates file data ranges with filenames.

Writes to `.ts` files are intercepted and forwarded to connected TCP clients.

Writes whose purpose cannot yet be determined may be temporarily quarantined. Filesystem metadata changes trigger a rescan, allowing newly-created files to be identified and their pending writes replayed through the appropriate path.

A slow TCP client is disconnected rather than blocking storage I/O.

## `gadget.sh`

`gadget.sh` prepares the fake disk and configures the USB Mass Storage gadget.

Commands:

```text
prepare-fakedisk
prepare-tssniff
stop-tssniff
mount
unmount
prepare-gadget
find-udc
start
stop
status
```

Typical usage:

```sh
sudo ./gadget.sh start
```

Check status:

```sh
sudo ./gadget.sh status
```

Stop:

```sh
sudo ./gadget.sh stop
```

For testing the exported filesystem without USB gadget mode:

```sh
sudo ./gadget.sh mount
```

and:

```sh
sudo ./gadget.sh unmount
```

## USB gadget requirements

USB gadget deployment requires:

* Linux
* FUSE
* ConfigFS
* `libcomposite`
* Mass Storage Gadget support
* A USB Device Controller (UDC)

The configured gadget exposes only a Mass Storage function:

```text
USB Mass Storage
└── LUN 0
    └── fake disk image
```

## Status

`tssniff` is experimental software for faking USB storage media and intercepting MPEG-TS file writes on Linux.
