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

if [ "$root" = / ] || [ ! -x "$root/lib/systemd/systemd" ] ||
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

units=$root/etc/systemd/system
network=$root/etc/systemd/network
install -d -m 0755 "$root/usr/local/bin" "$units/multi-user.target.wants" "$network"
for command in smolvm-agent taskd taskctl task-runner; do
    install -m 0755 "$bin/$command" "$root/usr/local/bin/$command"
done
for unit in smolvm-agent.service sandbox-taskd.service; do
    install -m 0644 "$source_dir/$unit" "$units/$unit"
    ln -sfn "../$unit" "$units/multi-user.target.wants/$unit"
done

printf '[Match]\nName=%s\n\n[Link]\nUnmanaged=yes\n' "$uplink" >"$network/05-smolvm-uplink.network"
chmod 0644 "$network/05-smolvm-uplink.network"
