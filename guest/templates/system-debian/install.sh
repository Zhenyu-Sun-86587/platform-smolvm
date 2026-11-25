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
ssh_dir=$root/etc/ssh/sandbox-managed
ssh_unit=sandbox-managed-sshd.service
ssh_sync=$root/usr/local/sbin/sandbox-system-ssh-sync

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

managed_ssh=no
if [ -x "$root/usr/sbin/sshd" ]; then
    for path in "$root/usr/sbin/sshd" "$ssh_dir" "$ssh_dir/sshd_config" \
        "$units/$ssh_unit" "$enabled/$ssh_unit" "$ssh_sync"; do
        check_guest_path "$path"
    done
    managed_ssh=yes
fi

install -d -m 0755 "$commands" "$enabled" "$network"
for command in smolvm-agent taskd taskctl task-runner; do
    install -T -m 0755 "$bin/$command" "$commands/$command"
done
for unit in smolvm-agent.service sandbox-taskd.service; do
    install -T -m 0644 "$source_dir/$unit" "$units/$unit"
    ln -sfn "../$unit" "$enabled/$unit"
done

if [ "$managed_ssh" = yes ]; then
    install -d -m 0755 "$(dirname -- "$ssh_sync")"
    install -d -m 0700 "$ssh_dir"
    install -T -m 0600 "$source_dir/sandbox-sshd_config" "$ssh_dir/sshd_config"
    install -T -m 0755 "$source_dir/sandbox-system-ssh-sync" "$ssh_sync"
    install -T -m 0644 "$source_dir/$ssh_unit" "$units/$ssh_unit"
    ln -sfn "../$ssh_unit" "$enabled/$ssh_unit"
fi

uplink_tmp=$(mktemp -- "$network/.smolvm-uplink.XXXXXXXX")
trap 'rm -f -- "$uplink_tmp"' EXIT
printf '[Match]\nName=%s\n\n[Link]\nUnmanaged=yes\n' "$uplink" >"$uplink_tmp"
chmod 0644 "$uplink_tmp"
mv -T -- "$uplink_tmp" "$uplink_config"
