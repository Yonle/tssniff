#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

BACKING_IMAGE="${BACKING_IMAGE:-/srv/guoxin.img}"
TSSNIFF_MOUNT="${TSSNIFF_MOUNT:-/mnt/tsdisk}"

TSSNIFF="${TSSNIFF:-tssniff}"
TSSNIFF_LISTEN="${TSSNIFF_LISTEN:-:6969}"
TSSNIFF_VERBOSE="${TSSNIFF_VERBOSE:-}"
TSSNIFF_DEBUG="${TSSNIFF_DEBUG:-}"

TSSNIFF_PIDFILE="${TSSNIFF_PIDFILE:-/run/guoxin-tssniff.pid}"
TSSNIFF_LOG="${TSSNIFF_LOG:-/var/log/guoxin-tssniff.log}"
TSSNIFF_START_TIMEOUT="${TSSNIFF_START_TIMEOUT:-15}"
TSSNIFF_STOP_TIMEOUT="${TSSNIFF_STOP_TIMEOUT:-15}"

FILESYSTEM="${FILESYSTEM:-ntfs}"
DISK_SIZE="${DISK_SIZE:-1T}"
SECTOR_SIZE=512
PARTITION_START_LBA=2048

CONFIGFS="${CONFIGFS:-/sys/kernel/config}"

PARTITION_OFFSET=$((PARTITION_START_LBA * SECTOR_SIZE))
PARTITION_TYPE=

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

die() {
    echo "error: $*" >&2
    exit 1
}

info() {
    echo "[+] $*"
}

need_root() {
    (( EUID == 0 )) || die "run as root"
}

configure_filesystem() {
    case "$FILESYSTEM" in
        fat32|vfat)
            FILESYSTEM=fat32
            PARTITION_TYPE=0c
            ;;

        exfat|ntfs)
            PARTITION_TYPE=07
            ;;

        *)
            die "unsupported filesystem: $FILESYSTEM"
            ;;
    esac
}

tssniff_running() {
    [[ -f "$TSSNIFF_PIDFILE" ]] || return 1

    local pid
    pid="$(cat "$TSSNIFF_PIDFILE" 2>/dev/null || true)"

    [[ -n "$pid" ]] &&
        kill -0 "$pid" 2>/dev/null
}

fuse_mounted() {
    mountpoint -q "$TSSNIFF_MOUNT"
}

# ---------------------------------------------------------------------------
# Fake disk
# ---------------------------------------------------------------------------

valid_image() {
    local image="$1"
    local loop type

    sfdisk --dump "$image" 2>/dev/null |
        grep -q '^label: dos$' || return 1

    loop="$(losetup --find --show --partscan "$image")" || return 1

    trap 'losetup -d "$loop" 2>/dev/null || true' RETURN

    [[ -b "${loop}p1" ]] || return 1

    type="$(
        blkid -s TYPE -o value "${loop}p1" 2>/dev/null || true
    )"

    case "$FILESYSTEM" in
        fat32)
            [[ "$type" == "vfat" ]]
            ;;
        exfat)
            [[ "$type" == "exfat" ]]
            ;;
        ntfs)
            [[ "$type" == "ntfs" ]]
            ;;
    esac
}

create_image() {
    local image="$1"
    local size sectors partition_sectors loop

    size="$(stat -c '%s' "$image")"

    (( size % SECTOR_SIZE == 0 )) ||
        die "image size is not sector-aligned: $size"

    sectors=$((size / SECTOR_SIZE))

    (( sectors > PARTITION_START_LBA )) ||
        die "image is too small"

    partition_sectors=$((sectors - PARTITION_START_LBA))

    info "creating MBR partition table"

    sfdisk "$image" <<EOF
label: dos
unit: sectors

start=${PARTITION_START_LBA}, size=${partition_sectors}, type=${PARTITION_TYPE}
EOF

    loop="$(
        losetup --find --show --partscan "$image"
    )"

    for _ in {1..20}; do
        [[ -b "${loop}p1" ]] && break
        sleep 0.1
    done

    [[ -b "${loop}p1" ]] ||
        die "partition device did not appear: ${loop}p1"

    case "$FILESYSTEM" in
        fat32)
            info "formatting FAT32"
            mkfs.fat \
                -F 32 \
                -s 128 \
                -n GUOXIN \
                "${loop}p1"
            ;;

        exfat)
            info "formatting exFAT"
            mkfs.exfat \
                -n GUOXIN \
                -c 128K \
                "${loop}p1"
            ;;

        ntfs)
            info "formatting NTFS"
            mkfs.ntfs \
                --quick \
                --label GUOXIN \
                "${loop}p1"
            ;;
    esac

    losetup -d "$loop"

    echo "fake disk prepared:"
    echo "  image:      $image"
    echo "  size:       $(stat -c '%s bytes' "$image")"
    echo "  filesystem: $FILESYSTEM"
    echo "  partition:  0x$PARTITION_TYPE"
    echo "  offset:     $PARTITION_OFFSET"
}

prepare_fakedisk() {
    need_root

    mkdir -p "$(dirname "$BACKING_IMAGE")"

    if [[ ! -e "$BACKING_IMAGE" ]]; then
        info "creating sparse $DISK_SIZE image"
        truncate -s "$DISK_SIZE" "$BACKING_IMAGE"
        create_image "$BACKING_IMAGE"
        return
    fi

    [[ -f "$BACKING_IMAGE" ]] ||
        die "backing image is not a regular file: $BACKING_IMAGE"

    if ! valid_image "$BACKING_IMAGE"; then
        die \
            "$BACKING_IMAGE exists but is not a valid MBR-partitioned $FILESYSTEM image; refusing to overwrite it"
    fi

    echo "fake disk already prepared:"
    echo "  image:      $BACKING_IMAGE"
    echo "  size:       $(stat -c '%s bytes' "$BACKING_IMAGE")"
    echo "  filesystem: $FILESYSTEM"
}

# ---------------------------------------------------------------------------
# Start
# ---------------------------------------------------------------------------

start() {
    need_root

    if tssniff_running; then
        echo "tssniff is already running (PID $(cat "$TSSNIFF_PIDFILE"))"
        return 0
    fi

    prepare_fakedisk

    command -v "$TSSNIFF" >/dev/null 2>&1 ||
        die "cannot find tssniff in PATH"

    if fuse_mounted; then
        info "removing stale FUSE mount"
        fusermount3 -u "$TSSNIFF_MOUNT" 2>/dev/null ||
            umount "$TSSNIFF_MOUNT" 2>/dev/null ||
            die "could not unmount stale FUSE mount"
    fi

    mkdir -p "$TSSNIFF_MOUNT"
    mkdir -p "$(dirname "$TSSNIFF_LOG")"

    info "loading libcomposite"
    modprobe libcomposite

    mkdir -p "$CONFIGFS"

    if ! mountpoint -q "$CONFIGFS"; then
        info "mounting ConfigFS"
        mount -t configfs none "$CONFIGFS"
    fi

    info "starting tssniff"

    local -a args=(
        -image "$BACKING_IMAGE"
        -mount "$TSSNIFF_MOUNT"
        -listen "$TSSNIFF_LISTEN"
        -fs "$FILESYSTEM"
    )

    [[ -n "$TSSNIFF_VERBOSE" ]] && args+=(-verbose)
    [[ -n "$TSSNIFF_DEBUG" ]] && args+=(-debug)

    "$TSSNIFF" "${args[@]}" \
        >"$TSSNIFF_LOG" \
        2>&1 &

    local pid=$!

    printf '%s\n' "$pid" >"$TSSNIFF_PIDFILE"

    for ((i = 0; i < TSSNIFF_START_TIMEOUT; i++)); do
        if [[ -e "$TSSNIFF_MOUNT/disk.img" ]]; then
            echo "tssniff started (PID $pid)"
            return 0
        fi

        if ! kill -0 "$pid" 2>/dev/null; then
            echo "---- tssniff log ----" >&2
            tail -n 80 "$TSSNIFF_LOG" 2>/dev/null || true
            echo "---------------------" >&2
            rm -f "$TSSNIFF_PIDFILE"
            die "tssniff exited during startup"
        fi

        sleep 1
    done

    die "timed out waiting for tssniff"
}

# ---------------------------------------------------------------------------
# Stop
# ---------------------------------------------------------------------------

stop() {
    need_root

    [[ -f "$TSSNIFF_PIDFILE" ]] ||
        die "tssniff is not running"

    local pid
    pid="$(cat "$TSSNIFF_PIDFILE" 2>/dev/null || true)"

    [[ -n "$pid" ]] ||
        die "invalid PID file"

    if kill -0 "$pid" 2>/dev/null; then
        info "stopping tssniff (PID $pid)"
        kill "$pid"

        for ((i = 0; i < TSSNIFF_STOP_TIMEOUT * 5; i++)); do
            kill -0 "$pid" 2>/dev/null || break
            sleep 0.2
        done

        if kill -0 "$pid" 2>/dev/null; then
            info "tssniff did not exit; sending SIGKILL"
            kill -KILL "$pid" 2>/dev/null || true
        fi
    fi

    rm -f "$TSSNIFF_PIDFILE"

    if fuse_mounted; then
        info "forcing FUSE unmount"
        fusermount3 -u "$TSSNIFF_MOUNT" 2>/dev/null ||
            umount -l "$TSSNIFF_MOUNT" 2>/dev/null ||
            true
    fi

    echo "tssniff stopped"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

configure_filesystem

case "${1:-}" in
    prepare-fakedisk)
        prepare_fakedisk
        ;;

    start)
        start
        ;;

    stop)
        stop
        ;;

    *)
        echo "usage: $0 {prepare-fakedisk|start|stop}" >&2
        exit 1
        ;;
esac
