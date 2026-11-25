#!/bin/sh
set -eu
if [ "$#" -ne 2 ]; then
    echo "usage: install.sh ROOTFS BINARY_DIR" >&2
    exit 2
fi
src=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
root=${1%/}
bin=$2
install -d -m 0755 "$root/usr/local/bin" "$root/usr/local/sbin" "$root/etc/ssh" "$root/workspace"
share=$root/usr/local/share/sandbox-managed
install -d -m 0755 "$share/services/taskd" "$share/services/sshd"
for command in taskd taskctl task-runner; do
    install -m 0755 "$bin/$command" "$root/usr/local/bin/$command"
done
install -m 0755 "$src/sandbox-bootstrap" "$src/sandbox-ssh-rotate" "$root/usr/local/sbin/"
install -m 0755 "$src/services/taskd/run" "$share/services/taskd/run"
install -m 0755 "$src/services/sshd/run" "$share/services/sshd/run"
install -m 0644 "$src/installation.json" "$share/installation.json"
install -m 0644 "$src/sshd_config" "$root/etc/ssh/sandbox_sshd_config"
install -d -m 0700 "$root/root/.ssh" "$root/var/lib/sandbox-taskd"
chmod 0700 "$root/root/.ssh" "$root/var/lib/sandbox-taskd"
rm -f "$root"/etc/ssh/ssh_host_*
