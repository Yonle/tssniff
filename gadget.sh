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
#       Start tssniff in the background.  tssniff mounts FUSE at
#       $TSSNIFF_MOUNT, exposes disk.img there, and configures the USB
#       Mass Storage gadget with disk.img as LUN 0.
#
#   stop-tssniff
#       Stop the tssniff instance started by this script.  tssniff tears
#       down its own USB gadget and unmounts FUSE on shutdown.
#
#   mount
#       TEST ONLY:
#       Mount partition 1 of the FUSE-exposed disk.img locally.
#
#   unmount
#       TEST ONLY:
#       Unmount the test filesystem.
#
#   prepare-gadget
#       Load the libcomposite kernel module and mount ConfigFS.  Actual
#       gadget configuration is performed by tssniff itself.
#
#   find-udc
#       Find/print available USB Device Controllers.
#
#   start
#       prepare-fakedisk
#       prepare-gadget (libcomposite + configfs)
#       prepare-tssniff (which configures the gadget)
#
#   stop
#       Stop tssniff (which tears down its own gadget).
#
#   status
#       Show state of disk, FUSE mount, tssniff and gadget.
#
###############################################################################

###############################################################################
# Paths
###############################################################################

BACKING_IMAGE="${BACKING_IMAGE:-/srv/guoxin.img}"

# FUSE mount point where tssniff exposes disk.img.
TSSNIFF_MOUNT="${TSSNIFF_MOUNT:-/mnt/tsdisk}"

# The FUSE-exposed file that the gadget uses as LUN 0.
GADGET_IMAGE="${GADGET_IMAGE:-${TSSNIFF_MOUNT}/disk.img}"

# Where the local test mount goes.
TEST_MOUNT="${TEST_MOUNT:-/mnt/guoxin}"

###############################################################################
# Filesystem
###############################################################################

# Accepted values:
#
#   fat32
#   vfat   -> alias for fat32
#   exfat
#   ntfs
#
FILESYSTEM="${FILESYSTEM:-ntfs}"

TSSNIFF_FILESYSTEM=""
MOUNT_FILESYSTEM=""
BLKID_FILESYSTEM=""

###############################################################################
# tssniff
###############################################################################

TSSNIFF="${TSSNIFF:-tssniff}"

TSSNIFF_LISTEN="${TSSNIFF_LISTEN:-:6969}"
TSSNIFF_VERBOSE="${TSSNIFF_VERBOSE:-}"
TSSNIFF_DEBUG="${TSSNIFF_DEBUG:-}"

TSSNIFF_PIDFILE="${TSSNIFF_PIDFILE:-/run/guoxin-tssniff.pid}"
TSSNIFF_LOG="${TSSNIFF_LOG:-/var/log/guoxin-tssniff.log}"

TSSNIFF_START_TIMEOUT="${TSSNIFF_START_TIMEOUT:-15}"
TSSNIFF_STOP_TIMEOUT="${TSSNIFF_STOP_TIMEOUT:-15}"

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
# The gadget name, descriptors, VID/PID, and UDC selection are configured
# inside tssniff (see usb_gadget.go).  GADGET_NAME here must match the
# constant in usb_gadget.go for `status` to find it.
###############################################################################

GADGET_NAME="${GADGET_NAME:-tsdisk}"

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

###############################################################################
# Filesystem configuration
###############################################################################

configure_filesystem() {
    case "$FILESYSTEM" in
        fat32|vfat)
            TSSNIFF_FILESYSTEM="fat32"
            MOUNT_FILESYSTEM="vfat"
            BLKID_FILESYSTEM="vfat"

            if [[ -z "$PARTITION_TYPE" ]]; then
                PARTITION_TYPE=0c
            fi
            ;;

        exfat)
            TSSNIFF_FILESYSTEM="exfat"
            MOUNT_FILESYSTEM="exfat"
            BLKID_FILESYSTEM="exfat"

            if [[ -z "$PARTITION_TYPE" ]]; then
                PARTITION_TYPE=7
            fi
            ;;

        ntfs)
            TSSNIFF_FILESYSTEM="ntfs"
            MOUNT_FILESYSTEM="ntfs"
            BLKID_FILESYSTEM="ntfs"

            if [[ -z "$PARTITION_TYPE" ]]; then
                PARTITION_TYPE=7
            fi
            ;;

        *)
            die \
                "unsupported filesystem: $FILESYSTEM (use fat32, vfat, exfat, or ntfs)"
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
# FUSE helpers
###############################################################################

fuse_disk_ready() {
    [[ -e "$GADGET_IMAGE" ]]
}

fuse_disk_size() {
    stat -c '%s' "$GADGET_IMAGE" 2>/dev/null || echo 0
}

fuse_mountpoint_active() {
    mountpoint -q "$TSSNIFF_MOUNT"
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

        ntfs)
            info "formatting partition 1 as NTFS"

            # --quick is mandatory to avoid zeroing the entire sparse image,
            # which would allocate the full DISK_SIZE on the host disk!
            if ! mkfs.ntfs \
                --quick \
                --label GUOXIN \
                "${loop}p1"
            then
                losetup -d "$loop" 2>/dev/null || true

                die \
                    "failed to format ${loop}p1 as NTFS"
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

    ###########################################################################
    # Already running?
    ###########################################################################

    if tssniff_is_running; then
        local pid

        pid="$(cat "$TSSNIFF_PIDFILE")"

        info "tssniff already running (PID $pid)"

        if fuse_disk_ready; then
            echo "  FUSE disk: $GADGET_IMAGE ($(fuse_disk_size) bytes)"
            return
        fi

        info "waiting for FUSE disk: $GADGET_IMAGE"

        wait_for_tssniff
        return
    fi

    ###########################################################################
    # Clean stale PID file.
    ###########################################################################

    rm -f "$TSSNIFF_PIDFILE"

    ###########################################################################
    # Clean up any leftover FUSE mount.
    ###########################################################################

    if fuse_mountpoint_active; then
        warn "stale FUSE mount at $TSSNIFF_MOUNT; attempting to unmount"
        fusermount3 -u "$TSSNIFF_MOUNT" 2>/dev/null \
            || umount "$TSSNIFF_MOUNT" 2>/dev/null \
            || die "could not unmount stale FUSE at $TSSNIFF_MOUNT"
    fi

    mkdir -p "$TSSNIFF_MOUNT"

    ###########################################################################
    # Start.
    #
    # tssniff mounts FUSE at $TSSNIFF_MOUNT, exposes disk.img there, and
    # configures the USB gadget with disk.img as LUN 0.  This script only
    # ensures libcomposite is loaded before tssniff starts.
    ###########################################################################

    info "starting tssniff"

    local -a args=(
        -image "$BACKING_IMAGE"
        -mount "$TSSNIFF_MOUNT"
        -listen "$TSSNIFF_LISTEN"
        -fs "$TSSNIFF_FILESYSTEM"
    )

    if [[ -n "$TSSNIFF_VERBOSE" ]]; then
        args+=(-verbose)
    fi

    if [[ -n "$TSSNIFF_DEBUG" ]]; then
        args+=(-debug)
    fi

    "$TSSNIFF" "${args[@]}" \
        > "$TSSNIFF_LOG" \
        2>&1 &

    local pid=$!

    printf '%s\n' "$pid" > "$TSSNIFF_PIDFILE"

    echo "  PID:        $pid"
    echo "  log:        $TSSNIFF_LOG"
    echo "  mount:      $TSSNIFF_MOUNT"
    echo "  disk:       $GADGET_IMAGE"
    echo "  listen:     $TSSNIFF_LISTEN"
    echo "  filesystem: $TSSNIFF_FILESYSTEM"

    wait_for_tssniff
}

wait_for_tssniff() {
    local i

    info "waiting for FUSE disk"

    for ((i = 0; i < TSSNIFF_START_TIMEOUT; i++)); do

        if fuse_disk_ready; then
            echo "  ready: $GADGET_IMAGE ($(fuse_disk_size) bytes)"
            return 0
        fi

        if ! tssniff_is_running; then
            warn "tssniff exited before creating the FUSE disk"

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
        "timed out waiting for $GADGET_IMAGE"
}

stop_tssniff() {
    need_root

    ###########################################################################
    # Kill tssniff.  It tears down its own USB gadget and unmounts FUSE on
    # SIGTERM (see main.go's signal handler and deferred Teardown / Unmount).
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

        for ((i = 0; i < TSSNIFF_STOP_TIMEOUT * 5; i++)); do
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

    ###########################################################################
    # If FUSE is still mounted (SIGKILL path), force-unmount it.
    ###########################################################################

    if fuse_mountpoint_active; then
        warn "forcing FUSE unmount at $TSSNIFF_MOUNT"
        fusermount3 -u "$TSSNIFF_MOUNT" 2>/dev/null \
            || umount -l "$TSSNIFF_MOUNT" 2>/dev/null \
            || true
    fi

    echo "tssniff stopped"
}

###############################################################################
# Test mount
###############################################################################

test_mount() {
    need_root

    fuse_disk_ready ||
        die \
            "FUSE disk is not ready: $GADGET_IMAGE
Run: $0 prepare-tssniff"

    mkdir -p "$TEST_MOUNT"

    if mountpoint -q "$TEST_MOUNT"; then
        echo "already mounted:"
        findmnt -T "$TEST_MOUNT"
        return
    fi

    info "mounting FUSE disk"
    info "filesystem: $MOUNT_FILESYSTEM"
    info "source:     $GADGET_IMAGE"
    info "target:     $TEST_MOUNT"

    mount \
        -t "$MOUNT_FILESYSTEM" \
        -o "loop,offset=${PARTITION_OFFSET},sync" \
        "$GADGET_IMAGE" \
        "$TEST_MOUNT"

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

    info "STEP 1/3: prepare fake disk"
    prepare_fakedisk

    echo

    info "STEP 2/3: prepare libcomposite"
    prepare_gadget

    echo

    info "STEP 3/3: start tssniff (configures gadget itself)"
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
        echo "  mount:   $TSSNIFF_MOUNT"
        echo "  listen:  $TSSNIFF_LISTEN"
        echo "  fs:      $TSSNIFF_FILESYSTEM"
        echo "  log:     $TSSNIFF_LOG"
    else
        echo "  state:   stopped"
    fi

    echo

    ###########################################################################
    # FUSE mount
    ###########################################################################

    echo "=== FUSE mount ==="

    echo "  mount:   $TSSNIFF_MOUNT"

    if fuse_mountpoint_active; then
        echo "  state:   mounted"
        findmnt -T "$TSSNIFF_MOUNT" || true
    else
        echo "  state:   not mounted"
    fi

    echo "  disk:    $GADGET_IMAGE"

    if fuse_disk_ready; then
        echo "  size:    $(fuse_disk_size) bytes"
    else
        echo "  MISSING"
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
        echo "  inactive (expected at $GADGET)"
        echo
        echo "  note: GADGET_NAME must match the constant in usb_gadget.go."
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
      fat32 (default), vfat, exfat, or ntfs

  BACKING_IMAGE=/srv/guoxin.img
  TSSNIFF_MOUNT=/mnt/tsdisk
  GADGET_IMAGE=/mnt/tsdisk/disk.img
  TEST_MOUNT=/mnt/guoxin

  TSSNIFF=tssniff
  TSSNIFF_LISTEN=:6969
  TSSNIFF_VERBOSE=
  TSSNIFF_DEBUG=
  TSSNIFF_PIDFILE=/run/guoxin-tssniff.pid
  TSSNIFF_LOG=/var/log/guoxin-tssniff.log
  TSSNIFF_START_TIMEOUT=15
  TSSNIFF_STOP_TIMEOUT=15

  DISK_SIZE=1T
  PARTITION_START_LBA=2048

  GADGET_NAME=tsdisk
      must match the constant in usb_gadget.go
  UDC=<udc-name>

Examples:

  sudo $0 prepare-fakedisk

  sudo FILESYSTEM=fat32 $0 start

  sudo FILESYSTEM=vfat $0 start

  sudo FILESYSTEM=exfat $0 start

  sudo FILESYSTEM=ntfs $0 start

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
