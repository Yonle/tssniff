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
#   mount
#       TEST ONLY:
#       Mount the FUSE-exposed disk.img using the selected filesystem.
#
#   unmount
#       TEST ONLY:
#       Unmount the test filesystem.
#
#   prepare-tssniff
#       Start tssniff in the background and wait for its FUSE disk.
#
#   stop-tssniff
#       Stop the tssniff instance started by this script.
#
#   prepare-gadget
#       Create the ConfigFS USB Mass Storage gadget.
#       Does NOT bind it to a UDC.
#
#   find-udc
#       Find/print available USB Device Controllers.
#
#   start
#       prepare-fakedisk
#       prepare-tssniff
#       prepare-gadget
#       find-udc
#       bind gadget
#
#   stop
#       Unbind/remove gadget and stop tssniff.
#
#   status
#       Show state of disk, tssniff, FUSE and gadget.
#
###############################################################################

###############################################################################
# Paths
###############################################################################

BACKING_IMAGE="${BACKING_IMAGE:-/srv/guoxin.img}"

# This file is created by tssniff's FUSE filesystem.
GADGET_IMAGE="${GADGET_IMAGE:-/mnt/tsdisk/disk.img}"

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

TSSNIFF_MOUNT="${TSSNIFF_MOUNT:-/mnt/tsdisk}"
TSSNIFF_LISTEN="${TSSNIFF_LISTEN:-:6969}"

TSSNIFF_PIDFILE="${TSSNIFF_PIDFILE:-/run/guoxin-tssniff.pid}"

TSSNIFF_START_TIMEOUT="${TSSNIFF_START_TIMEOUT:-15}"

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
###############################################################################

GADGET_NAME="${GADGET_NAME:-guoxin}"

CONFIGFS="${CONFIGFS:-/sys/kernel/config}"
GADGET="${CONFIGFS}/usb_gadget/${GADGET_NAME}"

UDC="${UDC:-}"

VID="${VID:-0x1d6b}"
PID="${PID:-0x0104}"

BCD_USB="${BCD_USB:-0x0200}"
BCD_DEVICE="${BCD_DEVICE:-0x0100}"

SERIAL="${SERIAL:-GUOXIN-TS-0001}"
MANUFACTURER="${MANUFACTURER:-Yonle Lab}"
PRODUCT="${PRODUCT:-USB Mass Storage}"
CONFIGURATION="${CONFIGURATION:-Mass Storage}"

MAX_POWER="${MAX_POWER:-250}"

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

    mkdir -p "$TSSNIFF_MOUNT"

    ###########################################################################
    # Already running?
    ###########################################################################

    if tssniff_is_running; then
        local pid

        pid="$(cat "$TSSNIFF_PIDFILE")"

        info "tssniff already running (PID $pid)"

        if [[ -e "$GADGET_IMAGE" ]]; then
            echo "  FUSE disk: $GADGET_IMAGE"
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
    # Make sure an old FUSE mount isn't sitting around.
    ###########################################################################

    if mountpoint -q "$TSSNIFF_MOUNT"; then
        die \
            "$TSSNIFF_MOUNT is already mounted; refusing to start tssniff over it"
    fi

    ###########################################################################
    # Start.
    ###########################################################################

    info "starting tssniff"

    "$TSSNIFF" \
        -image "$BACKING_IMAGE" \
        -mount "$TSSNIFF_MOUNT" \
        -listen "$TSSNIFF_LISTEN" \
        -fs "$TSSNIFF_FILESYSTEM" \
        > /var/log/guoxin-tssniff.log \
        2>&1 &

    local pid=$!

    printf '%s\n' "$pid" > "$TSSNIFF_PIDFILE"

    echo "  PID:        $pid"
    echo "  log:        /var/log/guoxin-tssniff.log"
    echo "  mount:      $TSSNIFF_MOUNT"
    echo "  listen:     $TSSNIFF_LISTEN"
    echo "  filesystem: $TSSNIFF_FILESYSTEM"

    wait_for_tssniff
}

wait_for_tssniff() {
    local i

    info "waiting for tssniff FUSE disk"

    for ((i = 0; i < TSSNIFF_START_TIMEOUT; i++)); do

        if [[ -e "$GADGET_IMAGE" ]]; then
            echo "  ready: $GADGET_IMAGE"
            return 0
        fi

        if ! tssniff_is_running; then
            warn "tssniff exited before creating the FUSE disk"

            echo
            echo "---- tssniff log ----"

            if [[ -f /var/log/guoxin-tssniff.log ]]; then
                tail -n 80 /var/log/guoxin-tssniff.log
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
        "timed out waiting for tssniff to create $GADGET_IMAGE"
}

stop_tssniff() {
    need_root

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

    [[ -e "$GADGET_IMAGE" ]] ||
        die \
            "FUSE disk does not exist: $GADGET_IMAGE
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

select_udc() {
    if [[ -n "$UDC" ]]; then
        [[ -d "/sys/class/udc/$UDC" ]] ||
            die "specified UDC does not exist: $UDC"

        return
    fi

    local -a udcs=()

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

    if (( ${#udcs[@]} > 1 )); then
        warn "multiple UDCs found:"
        printf '    %s\n' "${udcs[@]}"
        warn "using first: ${udcs[0]}"
        warn "override with UDC=<name>"
    fi

    UDC="${udcs[0]}"
}

###############################################################################
# Gadget ConfigFS
###############################################################################

prepare_gadget() {
    need_root

    modprobe libcomposite
    ensure_configfs

    [[ -e "$GADGET_IMAGE" ]] ||
        die \
            "FUSE disk does not exist: $GADGET_IMAGE
Run: $0 prepare-tssniff"

    if [[ -d "$GADGET" ]]; then
        local current=""

        if [[ -f "$GADGET/UDC" ]]; then
            current="$(cat "$GADGET/UDC" 2>/dev/null || true)"
        fi

        if [[ -n "$current" ]]; then
            die \
                "gadget is already bound to UDC: $current"
        fi

        warn "stale unbound gadget found; removing it"

        remove_gadget
    fi

    info "creating ConfigFS gadget: $GADGET_NAME"

    mkdir -p "$GADGET"

    ###########################################################################
    # Device descriptor
    ###########################################################################

    write_attr "$VID" \
        "$GADGET/idVendor"

    write_attr "$PID" \
        "$GADGET/idProduct"

    write_attr "$BCD_USB" \
        "$GADGET/bcdUSB"

    write_attr "$BCD_DEVICE" \
        "$GADGET/bcdDevice"

    ###########################################################################
    # Strings
    ###########################################################################

    mkdir -p \
        "$GADGET/strings/0x409"

    write_attr "$SERIAL" \
        "$GADGET/strings/0x409/serialnumber"

    write_attr "$MANUFACTURER" \
        "$GADGET/strings/0x409/manufacturer"

    write_attr "$PRODUCT" \
        "$GADGET/strings/0x409/product"

    ###########################################################################
    # Configuration
    ###########################################################################

    mkdir -p \
        "$GADGET/configs/c.1/strings/0x409"

    write_attr "$CONFIGURATION" \
        "$GADGET/configs/c.1/strings/0x409/configuration"

    # Self-powered.
    write_attr "0xC0" \
        "$GADGET/configs/c.1/bmAttributes"

    write_attr "$MAX_POWER" \
        "$GADGET/configs/c.1/MaxPower"

    ###########################################################################
    # Mass Storage
    ###########################################################################

    mkdir -p \
        "$GADGET/functions/mass_storage.0"

    write_attr "$GADGET_IMAGE" \
        "$GADGET/functions/mass_storage.0/lun.0/file"

    write_attr "1" \
        "$GADGET/functions/mass_storage.0/lun.0/removable"

    ###########################################################################
    # Add MSC function
    ###########################################################################

    ln -s \
        "$GADGET/functions/mass_storage.0" \
        "$GADGET/configs/c.1/mass_storage.0"

    echo
    echo "gadget prepared but NOT bound:"
    echo
    echo "  gadget:        $GADGET_NAME"
    echo "  image:         $GADGET_IMAGE"
    echo "  filesystem:    $FILESYSTEM"
    echo
    echo "  VID:           $VID"
    echo "  PID:           $PID"
    echo "  USB:           $BCD_USB"
    echo "  device:        $BCD_DEVICE"
    echo
    echo "  manufacturer:  $MANUFACTURER"
    echo "  product:       $PRODUCT"
    echo "  serial:        $SERIAL"
    echo "  configuration: $CONFIGURATION"
}

###############################################################################
# Bind
###############################################################################

bind_gadget() {
    need_root

    [[ -d "$GADGET" ]] ||
        die \
            "gadget has not been prepared; run prepare-gadget first"

    select_udc

    local current=""

    if [[ -f "$GADGET/UDC" ]]; then
        current="$(cat "$GADGET/UDC" 2>/dev/null || true)"
    fi

    if [[ -n "$current" ]]; then
        echo "gadget already bound to: $current"
        return
    fi

    info "binding $GADGET_NAME to UDC: $UDC"

    write_attr "$UDC" \
        "$GADGET/UDC"

    echo
    echo "========================================"
    echo " USB GADGET ACTIVE"
    echo "========================================"
    echo
    echo "  Gadget:     $GADGET_NAME"
    echo "  UDC:        $UDC"
    echo "  Image:      $GADGET_IMAGE"
    echo "  Filesystem: $FILESYSTEM"
    echo "  VID:        $VID"
    echo "  PID:        $PID"
    echo
}

###############################################################################
# Remove gadget
###############################################################################

remove_gadget() {
    [[ -d "$GADGET" ]] || return 0

    ###########################################################################
    # Unbind.
    ###########################################################################

    if [[ -f "$GADGET/UDC" ]]; then
        local current=""

        current="$(cat "$GADGET/UDC" 2>/dev/null || true)"

        if [[ -n "$current" ]]; then
            info "unbinding UDC: $current"

            write_attr "" \
                "$GADGET/UDC"
        fi
    fi

    ###########################################################################
    # Remove MSC from configuration.
    ###########################################################################

    rm -f \
        "$GADGET/configs/c.1/mass_storage.0" \
        2>/dev/null || true

    ###########################################################################
    # Remove MSC function.
    ###########################################################################

    rmdir \
        "$GADGET/functions/mass_storage.0" \
        2>/dev/null || true

    ###########################################################################
    # Remove configuration strings.
    ###########################################################################

    rmdir \
        "$GADGET/configs/c.1/strings/0x409" \
        2>/dev/null || true

    ###########################################################################
    # Remove configuration.
    ###########################################################################

    rmdir \
        "$GADGET/configs/c.1" \
        2>/dev/null || true

    ###########################################################################
    # Remove device strings.
    ###########################################################################

    rmdir \
        "$GADGET/strings/0x409" \
        2>/dev/null || true

    ###########################################################################
    # Remove gadget.
    ###########################################################################

    rmdir \
        "$GADGET" \
        2>/dev/null || true
}

###############################################################################
# Start
###############################################################################

start_gadget() {
    need_root

    info "STEP 1/4: prepare fake disk"
    prepare_fakedisk

    echo

    info "STEP 2/4: start tssniff"
    prepare_tssniff

    echo

    info "STEP 3/4: prepare gadget"
    prepare_gadget

    echo

    info "STEP 4/4: find UDC"
    select_udc

    echo "  selected UDC: $UDC"

    echo

    info "binding gadget"
    bind_gadget
}

###############################################################################
# Stop
###############################################################################

stop_gadget() {
    need_root

    if [[ -d "$GADGET" ]]; then
        info "stopping gadget"
        remove_gadget
        echo "gadget stopped"
    else
        echo "gadget is not present"
    fi

    echo

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
    else
        echo "  state:   stopped"
    fi

    echo

    ###########################################################################
    # FUSE disk
    ###########################################################################

    echo "=== FUSE disk ==="

    if [[ -e "$GADGET_IMAGE" ]]; then
        echo "  image:   $GADGET_IMAGE"
        echo "  size:    $(stat -c '%s bytes' "$GADGET_IMAGE")"
    else
        echo "  MISSING: $GADGET_IMAGE"
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
    # Gadget
    ###########################################################################

    echo "=== Gadget ==="

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
  GADGET_IMAGE=/mnt/tsdisk/disk.img
  TEST_MOUNT=/mnt/guoxin

  TSSNIFF=tssniff
  TSSNIFF_MOUNT=/mnt/tsdisk
  TSSNIFF_LISTEN=:6969
  TSSNIFF_PIDFILE=/run/guoxin-tssniff.pid
  TSSNIFF_START_TIMEOUT=15

  DISK_SIZE=1T
  PARTITION_START_LBA=2048

  UDC=<udc-name>

  VID=0x1d6b
  PID=0x0104

  BCD_USB=0x0200
  BCD_DEVICE=0x0100

  SERIAL=GUOXIN-TS-0001
  MANUFACTURER="Yonle Lab"
  PRODUCT="USB Mass Storage"
  CONFIGURATION="Mass Storage"
  MAX_POWER=250

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
