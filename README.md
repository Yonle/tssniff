# tssniff

`tssniff` is a Linux userspace utility that exposes a virtual filesystem through FUSE and monitors writes to MPEG-TS (`.ts`) files.

It is intended for use behind a USB Mass Storage gadget, where a host device sees a normal storage medium while selected TS file writes can be intercepted and forwarded over TCP.

## Architecture

```text
Host / STB
    │
    │ USB Mass Storage
    ▼
FUSE filesystem
    │
    ▼
tssniff
 ┌──┴────────────────┐
 │                   │
 │ normal files      │ *.ts writes
 │                   │
 ▼                   ▼
storage          interception
                     │
                     ▼
                 TCP :6969
```

The backing storage is an image file. `tssniff` creates and mounts a FUSE-visible disk image which can then be used as the backing file for a USB Mass Storage gadget.

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
| `-image`  | Backing storage image |
| `-mount`  | FUSE mount point      |
| `-listen` | TCP listen address    |

For example:

```sh
ffmpeg -re -i input.ts \
    -c copy \
    -f mpegts \
    /mnt/tsdisk/test.ts
```

A TCP client can connect to:

```sh
nc 192.168.50.1 6969
```

## Storage

The backing image is not directly exported to the USB host.

Instead:

```text
backing image
     │
     ▼
  tssniff
     │
     ▼
FUSE filesystem
     │
     ▼
gadget Mass Storage LUN
```

This allows filesystem operations to be observed and handled before reaching the backing storage.

## TS interception

Writes targeting `.ts` files are tracked by `tssniff`. Newly written ranges can be quarantined and replayed through the interception path.

The TCP output carries the intercepted MPEG-TS data as a byte stream. Transport framing is intentionally minimal; applications consuming the stream are expected to handle MPEG-TS accordingly.

## Requirements

* Linux
* FUSE
* A filesystem image suitable for the exported storage
* TCP networking

For USB gadget deployment, the Linux system additionally requires:

* ConfigFS
* `libcomposite`
* A USB Device Controller (UDC)
* Mass Storage Gadget support

A normal USB host controller is not sufficient for gadget mode.

## Intended deployment

A typical deployment can use:

```text
USB Gadget
    │
    │ Mass Storage only
    ▼
STB

Linux device
 ├── hostapd
 ├── tssniff :6969
 └── ConfigFS USB Mass Storage Gadget
```

The Wi-Fi network provides access to the TCP stream while the USB interface presents only a Mass Storage device to the STB.

## Status

`tssniff` is experimental software intended for Linux-based storage interception and USB-gadget applications.
