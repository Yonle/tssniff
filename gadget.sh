#!/usr/bin/env bash
set -euo pipefail

###############################################################################
# Guoxin USB gadget
#
# Commands:
#
#   prepare-fakedisk
#       Create/check the real sparse backing image.
#
#   prepare-tssniff
#       modprobe nbd, start tssniff in the background (NBD server + nbd-client
#       attachment + USB gadget configuration), and wait for /dev/nbdN to
#       become ready.
#
#   stop-tssniff
#       Stop the tssniff instance started by this script.  tssniff tears down
#       its own USB gadget on shutdown.
#
#   mount
#       TEST ONLY:
#       Mount partition 1 of the NBD device using the selected filesystem.
#
#   unmount
#       TEST ONLY:
#       Unmount the test filesystem.
#
#   prepare-gadget
#       Load the libcomposite kernel module and mount ConfigFS.  All gadget
#       configuration (descriptors, functions, UDC bind) is performed by
#       tssniff itself.
#
#   find-udc
#       Find/print available USB Device Controllers.
#
#   start
#       prepare-fakedisk
#       prepare-gadget (libcomposite only)
#       prepare-tssniff (which configures the gadget)
#
#   stop
#       Stop tssniff (which tears down its own gadget).
#
#   status
#       Show state of disk, NBD device, tssniff and gadget.
#
###############################################################################

###############################################################################
# Paths
###############################################################################

BACKING_IMAGE="${BACKING_IMAGE:-/srv/guoxin.img}"

# Kernel NBD device that tssniff attaches to its NBD server.
NBD_DEV="${NBD_DEV:-/dev/nbd0}"

# Where the test mount goes.
TEST_MOUNT="${TEST_MOUNT:-/mnt/guoxin}"

###############################################################################
# Filesystem
###############################################################################

# Accepted values:
#
#   fat32
#   vfat   -> alias for fat32
#   exfat
#
FILESYSTEM="${FILESYSTEM:-fat32}"

TSSNIFF_FILESYSTEM=""
MOUNT_FILESYSTEM=""
BLKID_FILESYSTEM=""

###############################################################################
# tssniff
###############################################################################

TSSNIFF="${TSSNIFF:-tssniff}"

TSSNIFF_LISTEN="${TSSNIFF_LISTEN:-:6969}"
TSSNIFF_VERBOSE="${TSSNIFF_VERBOSE:-}"

TSSNIFF_PIDFILE="${TSSNIFF_PIDFILE:-/run/guoxin-tssniff.pid}"
TSSNIFF_LOG="${TSSNIFF_LOG:-/var/log/guoxin-tssniff.log}"

TSSNIFF_START_TIMEOUT="${TSSNIFF_START_TIMEOUT:-15}"

###############################################################################
# NBD kernel module
###############################################################################

NBD_MODULE_NBDS_MAX="${NBD_MODULE_NBDS_MAX:-4}"
NBD_MODULE_MAX_PART="${NBD_MODULE_MAX_PART:-16}"

###############################################################################
# Fake disk
###############################################################################

DISK_SIZE="${DISK_SIZE:-1T}"
SECTOR_SIZE="${SECTOR_SIZE:-512}"

PARTITION_START_LBA="${PARTITION_START_LBA:-2048}"
PARTITION_TYPE="${PARTITION_TYPE:-}"

PARTITION_OFFSET="$(
    echo $((PARTITION_START_LBA * SECTOR_SIZE))
)"

###############################################################################
# USB gadget
#
# NOTE:
#   VID, PID, and descriptor strings are configured by tssniff itself
#   (see usb_gadget.go).  They are not passed through this script.
###############################################################################

GADGET_NAME="${GADGET_NAME:-guoxin}"

CONFIGFS="${CONFIGFS:-/sys/kernel/config}"
GADGET="${CONFIGFS}/usb_gadget/${GADGET_NAME}"

UDC="${UDC:-}"

###############################################################################
# Helpers
###############################################################################

die() {
    echo "error: $*" >&2
    exit 1
}

info() {
    echo "[+] $*"
}

warn() {
    echo "[!] $*" >&2
}

write_attr() {
    local value="$1"
    local path="$2"

    info "write $path = ${value:-<empty>}"

    printf '%s\n' "$value" > "$path"
}

###############################################################################
# Filesystem configuration
###############################################################################

configure_filesystem() {
    case "$FILESYSTEM" in
        fat32|vfat)
            # "vfat" is Linux's driver/filesystem name.
            # The on-disk filesystem and tssniff tracker are FAT32.
            TSSNIFF_FILESYSTEM="fat32"
            MOUNT_FILESYSTEM="vfat"
            BLKID_FILESYSTEM="vfat"

            if [[ -z "$PARTITION_TYPE" ]]; then
                # FAT32 LBA.
                PARTITION_TYPE=0c
            fi
            ;;

        exfat)
            TSSNIFF_FILESYSTEM="exfat"
            MOUNT_FILESYSTEM="exfat"
            BLKID_FILESYSTEM="exfat"

            if [[ -z "$PARTITION_TYPE" ]]; then
                # Microsoft basic data / exFAT.
                PARTITION_TYPE=7
            fi
            ;;

        *)
            die \
                "unsupported filesystem: $FILESYSTEM (use fat32, vfat, or exfat)"
            ;;
    esac
}

###############################################################################
# Root
###############################################################################

need_root() {
    [[ "${EUID}" -eq 0 ]] ||
        die "run this script as root"
}

###############################################################################
# ConfigFS
###############################################################################

ensure_configfs() {
    mkdir -p "$CONFIGFS"

    if ! mountpoint -q "$CONFIGFS"; then
        info "mounting ConfigFS"
        mount -t configfs none "$CONFIGFS"
    fi
}

###############################################################################
# NBD helpers
###############################################################################

nbd_device_ready() {
    [[ -b "$NBD_DEV" ]] || return 1

    local size
    size="$(blockdev --getsize64 "$NBD_DEV" 2>/dev/null || echo 0)"

    [[ "$size" -gt 0 ]]
}

nbd_device_size() {
    blockdev --getsize64 "$NBD_DEV" 2>/dev/null || echo 0
}

# Returns the NBD partition device if the kernel exposed one
# (requires max_part>=1 at modprobe time), otherwise falls back to the
# whole-disk device with an explicit offset.
detect_partition() {
    if [[ -b "${NBD_DEV}p1" ]]; then
        printf '%s\n' "${NBD_DEV}p1"
    else
        printf '%s\n' "$NBD_DEV"
    fi
}

###############################################################################
# NBD module load
###############################################################################

prepare_nbd_module() {
    need_root

    if [[ ! -b /dev/nbd0 ]]; then
        info "loading nbd module (nbds_max=${NBD_MODULE_NBDS_MAX}, max_part=${NBD_MODULE_MAX_PART})"

        if ! modprobe nbd \
            nbds_max="${NBD_MODULE_NBDS_MAX}" \
            max_part="${NBD_MODULE_MAX_PART}"
        then
            die \
                "failed to load nbd; install linux-modules-extra or enable CONFIG_BLK_DEV_NBD"
        fi

        # Give udev a moment to create the node.
        for _ in {1..20}; do
            [[ -b /dev/nbd0 ]] && break
            sleep 0.05
        done

        [[ -b /dev/nbd0 ]] ||
            die "nbd module loaded but /dev/nbd0 did not appear"
    fi

    if ! command -v nbd-client >/dev/null 2>&1; then
        die \
            "nbd-client is not installed; install the nbd-client package"
    fi
}

###############################################################################
# Fake disk
###############################################################################

is_partitioned_filesystem_image() {
    [[ -f "$1" ]] || return 1

    ###########################################################################
    # Must be an MBR disk.
    ###########################################################################

    if ! sfdisk --dump "$1" 2>/dev/null |
        grep -q '^label: dos$'
    then
        return 1
    fi

    ###########################################################################
    # Ask the kernel to expose the MBR partitions.
    ###########################################################################

    local loop
    local type

    loop="$(
        losetup \
            --find \
            --show \
            --partscan \
            "$1"
    )" || return 1

    if [[ ! -b "${loop}p1" ]]; then
        losetup -d "$loop" 2>/dev/null || true
        return 1
    fi

    type="$(
        blkid \
            -s TYPE \
            -o value \
            "${loop}p1" \
            2>/dev/null || true
    )"

    losetup -d "$loop" 2>/dev/null || true

    [[ "$type" == "$BLKID_FILESYSTEM" ]]
}

create_partitioned_filesystem_image() {
    local image="$1"

    local size
    local sectors
    local partition_sectors

    size="$(stat -c '%s' "$image")"

    if (( $size % $SECTOR_SIZE != 0 )); then
        die \
            "image size is not sector-aligned: $size bytes"
    fi

    sectors=$(($size / $SECTOR_SIZE))

    if (( $sectors <= $PARTITION_START_LBA )); then
        die \
            "disk is too small for partition starting at LBA $PARTITION_START_LBA"
    fi

    partition_sectors=$(($sectors - $PARTITION_START_LBA))

    info "creating MBR partition table"

    sfdisk \
        "$image" <<EOF
label: dos
unit: sectors

start=$PARTITION_START_LBA, size=$partition_sectors, type=$PARTITION_TYPE
EOF

    ###########################################################################
    # Attach the image and let Linux expose partition 1.
    ###########################################################################

    local loop

    loop="$(
        losetup \
            --find \
            --show \
            --partscan \
            "$image"
    )"

    echo "  loop:   $loop"
    echo "  start:  LBA $PARTITION_START_LBA"
    echo "  offset: $PARTITION_OFFSET bytes"

    ###########################################################################
    # Wait briefly for loopXp1 to appear.
    ###########################################################################

    for _ in {1..20}; do
        if [[ -b "${loop}p1" ]]; then
            break
        fi

        sleep 0.1
    done

    if [[ ! -b "${loop}p1" ]]; then
        losetup -d "$loop" 2>/dev/null || true

        die \
            "partition device did not appear: ${loop}p1"
    fi

    ###########################################################################
    # Format filesystem.
    ###########################################################################

    case "$FILESYSTEM" in
        fat32|vfat)
            info "formatting partition 1 as FAT32"

            if ! mkfs.fat \
                -F 32 \
                -s 128 \
                -n GUOXIN \
                "${loop}p1"
            then
                losetup -d "$loop" 2>/dev/null || true

                die \
                    "failed to format ${loop}p1 as FAT32"
            fi
            ;;

        exfat)
            info "formatting partition 1 as exFAT"

            if ! mkfs.exfat \
                -n GUOXIN \
                -c 128K \
                "${loop}p1"
            then
                losetup -d "$loop" 2>/dev/null || true

                die \
                    "failed to format ${loop}p1 as exFAT"
            fi
            ;;
    esac

    losetup -d "$loop"

    echo
    echo "partitioned fake disk prepared:"
    echo "  image:      $image"
    echo "  size:       $(stat -c '%s bytes' "$image")"
    echo "  partition:  1"
    echo "  type:       MBR 0x${PARTITION_TYPE}"
    echo "  start LBA:  $PARTITION_START_LBA"
    echo "  offset:     $PARTITION_OFFSET bytes"
    echo "  filesystem: $FILESYSTEM"
}

prepare_fakedisk() {
    need_root

    local parent
    parent="$(dirname "$BACKING_IMAGE")"

    mkdir -p "$parent"

    if [[ ! -e "$BACKING_IMAGE" ]]; then
        info "creating sparse ${DISK_SIZE} fake disk"
        info "backing image: $BACKING_IMAGE"

        truncate \
            -s "$DISK_SIZE" \
            "$BACKING_IMAGE"

        create_partitioned_filesystem_image \
            "$BACKING_IMAGE"

        return
    fi

    if [[ ! -f "$BACKING_IMAGE" ]]; then
        die \
            "backing image exists but is not a regular file: $BACKING_IMAGE"
    fi

    if ! is_partitioned_filesystem_image \
        "$BACKING_IMAGE"
    then
        die \
            "$BACKING_IMAGE exists but is not an MBR-partitioned $FILESYSTEM image; refusing to overwrite it"
    fi

    local size
    size="$(stat -c '%s' "$BACKING_IMAGE")"

    echo "fake disk already prepared:"
    echo "  image:      $BACKING_IMAGE"
    echo "  size:       $size bytes"
    echo "  partition:  MBR partition 1"
    echo "  type:    0x${PARTITION_TYPE}"
    echo "  start LBA:  $PARTITION_START_LBA"
    echo "  offset:     $PARTITION_OFFSET bytes"
    echo "  filesystem: $FILESYSTEM"
}

###############################################################################
# libcomposite / ConfigFS
###############################################################################

prepare_gadget() {
    need_root

    info "loading libcomposite"
    modprobe libcomposite

    ensure_configfs

    echo
    echo "libcomposite prepared:"
    echo "  module:   loaded"
    echo "  configfs: mounted at $CONFIGFS"
    echo
    echo "note: gadget descriptors, functions, and UDC bind are configured"
    echo "      by tssniff.  see prepare-tssniff / start."
}

###############################################################################
# tssniff
###############################################################################

tssniff_is_running() {
    [[ -f "$TSSNIFF_PIDFILE" ]] || return 1

    local pid

    pid="$(cat "$TSSNIFF_PIDFILE" 2>/dev/null || true)"

    [[ -n "$pid" ]] || return 1

    kill -0 "$pid" 2>/dev/null
}

prepare_tssniff() {
    need_root

    command -v "$TSSNIFF" >/dev/null 2>&1 ||
        die "cannot find tssniff in PATH"

    prepare_nbd_module

    ###########################################################################
    # Already running?
    ###########################################################################

    if tssniff_is_running; then
        local pid

        pid="$(cat "$TSSNIFF_PIDFILE")"

        info "tssniff already running (PID $pid)"

        if nbd_device_ready; then
            echo "  NBD device: $NBD_DEV ($(nbd_device_size) bytes)"
            return
        fi

        info "waiting for NBD device: $NBD_DEV"

        wait_for_tssniff
        return
    fi

    ###########################################################################
    # Clean stale PID file.
    ###########################################################################

    rm -f "$TSSNIFF_PIDFILE"

    ###########################################################################
    # If a previous run left /dev/nbdN attached, disconnect it first.
    ###########################################################################

    if nbd_device_ready; then
        warn "detaching stale NBD device: $NBD_DEV"
        nbd-client -d "$NBD_DEV" >/dev/null 2>&1 || true

        for _ in {1..20}; do
            nbd_device_ready || break
            sleep 0.1
        done
    fi

    ###########################################################################
    # Start.
    #
    # tssniff owns the NBD server, the nbd-client attachment, and the USB
    # gadget configuration (descriptors, functions, UDC bind).  This script
    # only ensures libcomposite is loaded before tssniff starts.
    ###########################################################################

    info "starting tssniff"

    local -a args=(
        -image "$BACKING_IMAGE"
        -listen "$TSSNIFF_LISTEN"
        -fs "$TSSNIFF_FILESYSTEM"
        -nbd "$NBD_DEV"
    )

    if [[ -n "$TSSNIFF_VERBOSE" ]]; then
        args+=(-verbose)
    fi

    "$TSSNIFF" "${args[@]}" \
        > "$TSSNIFF_LOG" \
        2>&1 &

    local pid=$!

    printf '%s\n' "$pid" > "$TSSNIFF_PIDFILE"

    echo "  PID:        $pid"
    echo "  log:        $TSSNIFF_LOG"
    echo "  NBD device: $NBD_DEV"
    echo "  listen:     $TSSNIFF_LISTEN"
    echo "  filesystem: $TSSNIFF_FILESYSTEM"

    wait_for_tssniff
}

wait_for_tssniff() {
    local i

    info "waiting for NBD device to become ready"

    for ((i = 0; i < TSSNIFF_START_TIMEOUT; i++)); do

        if nbd_device_ready; then
            echo "  ready: $NBD_DEV ($(nbd_device_size) bytes)"
            return 0
        fi

        if ! tssniff_is_running; then
            warn "tssniff exited before attaching the NBD device"

            echo
            echo "---- tssniff log ----"

            if [[ -f "$TSSNIFF_LOG" ]]; then
                tail -n 80 "$TSSNIFF_LOG"
            else
                echo "(no log)"
            fi

            echo "---------------------"

            rm -f "$TSSNIFF_PIDFILE"

            return 1
        fi

        sleep 1
    done

    die \
        "timed out waiting for $NBD_DEV to become ready"
}

stop_tssniff() {
    need_root

    ###########################################################################
    # Detach NBD device first, otherwise tssniff may block on shutdown while
    # NBD_DO_IT is still running.
    ###########################################################################

    if nbd_device_ready; then
        info "detaching NBD device: $NBD_DEV"
        nbd-client -d "$NBD_DEV" >/dev/null 2>&1 || true
    fi

    ###########################################################################
    # Kill tssniff.  tssniff removes its own USB gadget on shutdown.
    ###########################################################################

    if [[ ! -f "$TSSNIFF_PIDFILE" ]]; then
        echo "tssniff is not managed by this wrapper"
        return 0
    fi

    local pid

    pid="$(cat "$TSSNIFF_PIDFILE" 2>/dev/null || true)"

    if [[ -z "$pid" ]]; then
        rm -f "$TSSNIFF_PIDFILE"
        return 0
    fi

    if kill -0 "$pid" 2>/dev/null; then
        info "stopping tssniff (PID $pid)"

        kill "$pid"

        for _ in {1..10}; do
            if ! kill -0 "$pid" 2>/dev/null; then
                break
            fi

            sleep 0.2
        done

        if kill -0 "$pid" 2>/dev/null; then
            warn "tssniff did not exit; sending SIGKILL"
            kill -KILL "$pid" 2>/dev/null || true
        fi
    fi

    rm -f "$TSSNIFF_PIDFILE"

    echo "tssniff stopped"
}

###############################################################################
# Test mount
###############################################################################

test_mount() {
    need_root

    nbd_device_ready ||
        die \
            "NBD device is not ready: $NBD_DEV
Run: $0 prepare-tssniff"

    mkdir -p "$TEST_MOUNT"

    if mountpoint -q "$TEST_MOUNT"; then
        echo "already mounted:"
        findmnt -T "$TEST_MOUNT"
        return
    fi

    local source
    source="$(detect_partition)"

    info "mounting NBD device"
    info "filesystem: $MOUNT_FILESYSTEM"
    info "source:     $source"
    info "target:     $TEST_MOUNT"

    if [[ "$source" == "$NBD_DEV" ]]; then
        #######################################################################
        # Kernel did not expose a partition node, use explicit offset.
        #######################################################################

        mount \
            -t "$MOUNT_FILESYSTEM" \
            -o "offset=${PARTITION_OFFSET},sync" \
            "$source" \
            "$TEST_MOUNT"
    else
        mount \
            -t "$MOUNT_FILESYSTEM" \
            -o "sync" \
            "$source" \
            "$TEST_MOUNT"
    fi

    echo
    echo "test filesystem mounted:"
    findmnt -T "$TEST_MOUNT"
}

test_unmount() {
    need_root

    if ! mountpoint -q "$TEST_MOUNT"; then
        echo "test filesystem is not mounted"
        return
    fi

    info "unmounting $TEST_MOUNT"

    umount "$TEST_MOUNT"
}

###############################################################################
# UDC
###############################################################################

find_udc() {
    need_root

    local -a udcs=()

    if [[ -n "$UDC" ]]; then
        [[ -d "/sys/class/udc/$UDC" ]] ||
            die "specified UDC does not exist: $UDC"

        printf '%s\n' "$UDC"
        return
    fi

    while IFS= read -r path; do
        udcs+=("$(basename "$path")")
    done < <(
        find /sys/class/udc \
            -mindepth 1 \
            -maxdepth 1 \
            -type l \
            2>/dev/null |
        sort
    )

    if (( ${#udcs[@]} == 0 )); then
        die \
            "no UDC found; this USB controller is not exposed as a gadget/peripheral controller"
    fi

    printf '%s\n' "${udcs[@]}"
}

###############################################################################
# Start
###############################################################################

start_gadget() {
    need_root

    info "STEP 1/4: prepare fake disk"
    prepare_fakedisk

    echo

    info "STEP 2/4: check nbd module"
    prepare_nbd_module

    echo

    info "STEP 3/4: prepare libcomposite"
    prepare_gadget

    echo

    info "STEP 4/4: start tssniff (configures gadget itself)"
    prepare_tssniff
}

###############################################################################
# Stop
###############################################################################

stop_gadget() {
    need_root

    stop_tssniff
}

###############################################################################
# Status
###############################################################################

status_gadget() {
    need_root
    ensure_configfs

    echo "========================================"
    echo " Guoxin Gadget Status"
    echo "========================================"
    echo

    ###########################################################################
    # Fake disk
    ###########################################################################

    echo "=== Fake disk ==="

    if [[ -f "$BACKING_IMAGE" ]]; then
        echo "  backing: $BACKING_IMAGE"
        echo "  size:    $(stat -c '%s bytes' "$BACKING_IMAGE")"

        if is_partitioned_filesystem_image "$BACKING_IMAGE"; then
            echo "  layout:  MBR"
            echo "  part:    1"

            printf \
                '  type:    0x%02x\n' \
                "$PARTITION_TYPE"

            echo "  fs:      $FILESYSTEM"
            echo "  offset:  ${PARTITION_OFFSET} bytes"
        else
            echo "  layout:  UNKNOWN"
        fi

        echo "  blocks:  $(du -h "$BACKING_IMAGE" | cut -f1)"
    else
        echo "  MISSING: $BACKING_IMAGE"
    fi

    echo

    ###########################################################################
    # tssniff
    ###########################################################################

    echo "=== tssniff ==="

    if tssniff_is_running; then
        echo "  state:   running"
        echo "  pid:     $(cat "$TSSNIFF_PIDFILE")"
        echo "  listen:  $TSSNIFF_LISTEN"
        echo "  fs:      $TSSNIFF_FILESYSTEM"
        echo "  log:     $TSSNIFF_LOG"
    else
        echo "  state:   stopped"
    fi

    echo

    ###########################################################################
    # NBD device
    ###########################################################################

    echo "=== NBD device ==="

    echo "  device:  $NBD_DEV"

    if [[ -b "$NBD_DEV" ]]; then
        local sz
        sz="$(nbd_device_size)"

        echo "  state:   present"
        echo "  size:    $sz bytes"

        if [[ -b "${NBD_DEV}p1" ]]; then
            echo "  part:    ${NBD_DEV}p1"
        else
            echo "  part:    (none; kernel partition scan disabled)"
        fi
    else
        echo "  state:   MISSING"
    fi

    echo

    ###########################################################################
    # Test mount
    ###########################################################################

    echo "=== Test mount ==="

    if mountpoint -q "$TEST_MOUNT"; then
        findmnt -T "$TEST_MOUNT"
    else
        echo "  unmounted"
    fi

    echo

    ###########################################################################
    # UDC
    ###########################################################################

    echo "=== UDC ==="

    local udc_output

    if udc_output="$(find_udc 2>/dev/null)"; then
        while IFS= read -r udc; do
            echo "  $udc"
        done <<< "$udc_output"
    else
        echo "  NONE"
    fi

    echo

    ###########################################################################
    # Gadget (configured by tssniff)
    ###########################################################################

    echo "=== Gadget (owned by tssniff) ==="

    if [[ ! -d "$GADGET" ]]; then
        echo "  inactive"
        return 0
    fi

    local bound=""

    if [[ -f "$GADGET/UDC" ]]; then
        bound="$(cat "$GADGET/UDC" 2>/dev/null || true)"
    fi

    echo "  name:   $GADGET_NAME"
    echo "  UDC:    ${bound:-unbound}"

    if [[ -L "$GADGET/configs/c.1/mass_storage.0" ]]; then
        echo "  MSC:    configured"

        local lun_file=""
        lun_file="$(cat "$GADGET/functions/mass_storage.0/lun.0/file" 2>/dev/null || true)"

        if [[ -n "$lun_file" ]]; then
            echo "  LUN 0:  $lun_file"
        fi
    else
        echo "  MSC:    NOT configured"
    fi
}

###############################################################################
# Usage
###############################################################################

usage() {
    cat <<EOF
usage:
  $0 prepare-fakedisk
  $0 prepare-tssniff
  $0 stop-tssniff
  $0 mount
  $0 unmount
  $0 prepare-gadget
  $0 find-udc
  $0 start
  $0 stop
  $0 status

Environment:

  FILESYSTEM=fat32
      fat32 (default), vfat, or exfat

  BACKING_IMAGE=/srv/guoxin.img
  NBD_DEV=/dev/nbd0
  TEST_MOUNT=/mnt/guoxin

  TSSNIFF=tssniff
  TSSNIFF_LISTEN=:6969
  TSSNIFF_VERBOSE=
  TSSNIFF_PIDFILE=/run/guoxin-tssniff.pid
  TSSNIFF_LOG=/var/log/guoxin-tssniff.log
  TSSNIFF_START_TIMEOUT=15

  NBD_MODULE_NBDS_MAX=4
  NBD_MODULE_MAX_PART=16

  DISK_SIZE=1T
  PARTITION_START_LBA=2048

  GADGET_NAME=guoxin
  UDC=<udc-name>

Examples:

  sudo $0 prepare-fakedisk

  sudo FILESYSTEM=fat32 $0 start

  sudo FILESYSTEM=vfat $0 start

  sudo FILESYSTEM=exfat $0 start

  sudo FILESYSTEM=fat32 $0 mount

  sudo $0 status

  sudo $0 stop

EOF
}

###############################################################################
# Main
###############################################################################

configure_filesystem

case "${1:-}" in
    prepare-fakedisk)
        prepare_fakedisk
        ;;

    prepare-tssniff)
        prepare_tssniff
        ;;

    stop-tssniff)
        stop_tssniff
        ;;

    mount)
        test_mount
        ;;

    unmount)
        test_unmount
        ;;

    prepare-gadget)
        prepare_gadget
        ;;

    find-udc)
        find_udc
        ;;

    start)
        start_gadget
        ;;

    stop)
        stop_gadget
        ;;

    status)
        status_gadget
        ;;

    help|-h|--help)
        usage
        ;;

    *)
        usage
        exit 1
        ;;
esac