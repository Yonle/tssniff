#!/usr/bin/env bash
set -euo pipefail

BACKING_IMAGE="${BACKING_IMAGE:-/srv/guoxin.img}"
TSSNIFF_MOUNT="${TSSNIFF_MOUNT:-/mnt/tsdisk}"

TSSNIFF="${TSSNIFF:-tssniff}"
TSSNIFF_LISTEN="${TSSNIFF_LISTEN:-:6969}"
TSSNIFF_VERBOSE="${TSSNIFF_VERBOSE:-}"
TSSNIFF_DEBUG="${TSSNIFF_DEBUG:-}"

PIDFILE="${TSSNIFF_PIDFILE:-/run/guoxin-tssniff.pid}"
LOGFILE="${TSSNIFF_LOG:-/var/log/guoxin-tssniff.log}"

DISK_SIZE="${DISK_SIZE:-1T}"

PARTITION_START_LBA=2048
SECTOR_SIZE=512
PARTITION_OFFSET=$((PARTITION_START_LBA * SECTOR_SIZE))

CONFIGFS="/sys/kernel/config"
GADGET="/sys/kernel/config/usb_gadget/tsdisk"

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

tssniff_running() {
	[[ -f "$PIDFILE" ]] || return 1

	local pid
	pid="$(cat "$PIDFILE" 2>/dev/null || true)"

	[[ -n "$pid" ]] &&
		kill -0 "$pid" 2>/dev/null
}

fuse_mounted() {
	mountpoint -q "$TSSNIFF_MOUNT"
}

valid_image() {
	local image="$1"
	local loop type

	sfdisk --dump "$image" 2>/dev/null |
		grep -q '^label: dos$' || return 1

	loop="$(
		losetup --find --show --partscan "$image"
	)" || return 1

	if [[ ! -b "${loop}p1" ]]; then
		losetup -d "$loop" 2>/dev/null || true
		return 1
	fi

	type="$(
		blkid -s TYPE -o value "${loop}p1" 2>/dev/null || true
	)"

	losetup -d "$loop" 2>/dev/null || true

	[[ "$type" == "ntfs" ]]
}

create_image() {
	local image="$1"
	local size sectors partition_sectors loop

	size="$(stat -c '%s' "$image")"

	(( size % SECTOR_SIZE == 0 )) ||
		die "image size is not sector-aligned"

	sectors=$((size / SECTOR_SIZE))

	(( sectors > PARTITION_START_LBA )) ||
		die "image is too small"

	partition_sectors=$((sectors - PARTITION_START_LBA))

	info "creating NTFS partition"

	sfdisk "$image" <<EOF
label: dos
unit: sectors

start=${PARTITION_START_LBA}, size=${partition_sectors}, type=07
EOF

	loop="$(
		losetup --find --show --partscan "$image"
	)"

	for _ in {1..20}; do
		[[ -b "${loop}p1" ]] && break
		sleep 0.1
	done

	if [[ ! -b "${loop}p1" ]]; then
		losetup -d "$loop" 2>/dev/null || true
		die "partition device did not appear"
	fi

	info "formatting NTFS"

	if ! mkfs.ntfs \
		--quick \
		--label GUOXIN \
		"${loop}p1"
	then
		losetup -d "$loop" 2>/dev/null || true
		die "failed to format NTFS"
	fi

	losetup -d "$loop"

	echo "NTFS image prepared:"
	echo "  image:  $image"
	echo "  size:   $(stat -c '%s bytes' "$image")"
	echo "  start:  LBA $PARTITION_START_LBA"
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
		die "backing image is not a regular file"

	if ! valid_image "$BACKING_IMAGE"; then
		die \
			"$BACKING_IMAGE is not a valid MBR/NTFS image; refusing to overwrite it"
	fi

	echo "NTFS image already prepared:"
	echo "  image:  $BACKING_IMAGE"
	echo "  size:   $(stat -c '%s bytes' "$BACKING_IMAGE")"
}

start() {
	need_root

	if tssniff_running; then
		echo "tssniff is already running (PID $(cat "$PIDFILE"))"
		return
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
	mkdir -p "$(dirname "$LOGFILE")"

	modprobe libcomposite

	mkdir -p "$CONFIGFS"

	if ! mountpoint -q "$CONFIGFS"; then
		mount -t configfs none "$CONFIGFS"
	fi

	info "starting tssniff"

	local -a args=(
		-image "$BACKING_IMAGE"
		-mount "$TSSNIFF_MOUNT"
		-listen "$TSSNIFF_LISTEN"
	)

	[[ -n "$TSSNIFF_VERBOSE" ]] &&
		args+=(-verbose)

	[[ -n "$TSSNIFF_DEBUG" ]] &&
		args+=(-debug)

	"$TSSNIFF" "${args[@]}" \
		>"$LOGFILE" \
		2>&1 &

	local pid=$!

	printf '%s\n' "$pid" >"$PIDFILE"

	for ((i = 0; i < 15; i++)); do
		if [[ -e "$TSSNIFF_MOUNT/disk.img" ]]; then
			echo "tssniff started (PID $pid)"
			return
		fi

		if ! kill -0 "$pid" 2>/dev/null; then
			echo "---- tssniff log ----" >&2
			tail -n 80 "$LOGFILE" 2>/dev/null || true
			echo "---------------------" >&2

			rm -f "$PIDFILE"
			die "tssniff exited during startup"
		fi

		sleep 1
	done

	die "timed out waiting for tssniff"
}

stop() {
	need_root

	if [[ ! -f "$PIDFILE" ]]; then
		echo "tssniff is not running"
		return
	fi

	local pid
	pid="$(cat "$PIDFILE" 2>/dev/null || true)"

	if [[ -n "$pid" ]] &&
		kill -0 "$pid" 2>/dev/null
	then
		info "stopping tssniff (PID $pid)"

		kill "$pid"

		for ((i = 0; i < 75; i++)); do
			kill -0 "$pid" 2>/dev/null || break
			sleep 0.2
		done

		if kill -0 "$pid" 2>/dev/null; then
			info "tssniff did not exit; sending SIGKILL"
			kill -KILL "$pid" 2>/dev/null || true
		fi
	fi

	rm -f "$PIDFILE"

	if fuse_mounted; then
		info "forcing FUSE unmount"

		fusermount3 -u "$TSSNIFF_MOUNT" 2>/dev/null ||
			umount -l "$TSSNIFF_MOUNT" 2>/dev/null ||
			true
	fi

	echo "tssniff stopped"
}

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
