#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
    echo "usage: install.sh DEBIAN_ROOTFS BINARY_DIR MANAGED_UPLINK" >&2
    exit 2
fi

source_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=$(realpath -e -- "$1")
bin=$(realpath -e -- "$2")
uplink=$3

if [ "$root" = / ]; then
    echo "use a prepared Debian rootfs with systemd installed" >&2
    exit 2
fi

check_guest_path() {
    guest_path=$1
    resolved=$(realpath -m -- "$guest_path")
    case "$resolved" in
        "$root"/*) ;;
        *)
            printf 'path escapes the Debian rootfs: %s\n' "${guest_path#"$root"/}" >&2
            exit 2 ;;
    esac
}

check_guest_path "$root/lib/systemd/systemd"
check_guest_path "$root/etc/os-release"
if [ ! -x "$root/lib/systemd/systemd" ] ||
    ! grep -qx 'ID=debian' "$root/etc/os-release"; then
    echo "use a prepared Debian rootfs with systemd installed" >&2
    exit 2
fi
case "$uplink" in
    ''|.|..|*[!a-zA-Z0-9_.-]*)
        echo "managed uplink must be a literal interface name" >&2
        exit 2 ;;
esac
if [ "${#uplink}" -gt 15 ]; then
    echo "managed uplink must be at most 15 bytes" >&2
    exit 2
fi

for command in smolvm-agent taskd taskctl task-runner; do
    if [ ! -x "$bin/$command" ]; then
        echo "missing executable $bin/$command" >&2
        exit 2
    fi
done

commands=$root/usr/local/bin
units=$root/etc/systemd/system
enabled=$units/multi-user.target.wants
network=$root/etc/systemd/network
uplink_config=$network/05-smolvm-uplink.network

for directory in "$commands" "$units" "$enabled" "$network"; do
    check_guest_path "$directory"
done
for command in smolvm-agent taskd taskctl task-runner; do
    check_guest_path "$commands/$command"
done
for unit in smolvm-agent.service sandbox-taskd.service; do
    check_guest_path "$units/$unit"
    check_guest_path "$enabled/$unit"
done
check_guest_path "$uplink_config"

install -d -m 0755 "$commands" "$enabled" "$network"
for command in smolvm-agent taskd taskctl task-runner; do
    install -T -m 0755 "$bin/$command" "$commands/$command"
done
for unit in smolvm-agent.service sandbox-taskd.service; do
    install -T -m 0644 "$source_dir/$unit" "$units/$unit"
    ln -sfn "../$unit" "$enabled/$unit"
done

uplink_tmp=$(mktemp -- "$network/.smolvm-uplink.XXXXXXXX")
trap 'rm -f -- "$uplink_tmp"' EXIT
printf '[Match]\nName=%s\n\n[Link]\nUnmanaged=yes\n' "$uplink" >"$uplink_tmp"
chmod 0644 "$uplink_tmp"
mv -T -- "$uplink_tmp" "$uplink_config"
