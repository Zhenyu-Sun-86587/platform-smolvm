#!/bin/bash
# Bootstrap only the disposable KVM host created by worker_host_restart_kvm.py.
set -Eeuo pipefail
grep -qw 'sandbox.release_fixture=isolated' /proc/cmdline
seed=/mnt/release-fixture
[[ $(findmnt -n -o SOURCE -T "$seed") == /dev/vdb ]]
phase=$2
[[ $phase == running || $phase == recovered ]]
trap 'echo "RELEASE_FIXTURE_FAILURE phase=$phase line=$LINENO"' ERR
if [[ $1 == bootstrap ]]; then
    if [[ $phase == running ]]; then
        resize2fs /dev/vda
        install -d "/lib/modules/$(uname -r)/extra"
        install -m 0644 "$seed"/modules/*.ko "/lib/modules/$(uname -r)/extra/"
        depmod -a
        cat > /etc/systemd/system/worker-host-fixture.service <<'UNIT'
[Unit]
Description=Validate a private Worker catalog across a host power cut
After=local-fs.target network.target

[Service]
Type=simple
ExecStart=/bin/bash /mnt/release-fixture/guest.sh run ${FIXTURE_PHASE}
EnvironmentFile=/run/worker-host-fixture.env
PrivateMounts=yes
PrivateNetwork=yes
StandardOutput=tty
StandardError=tty
TTYPath=/dev/ttyS0

[Install]
WantedBy=multi-user.target
UNIT
        ln -s ../worker-host-fixture.service /etc/systemd/system/multi-user.target.wants/worker-host-fixture.service
    fi
    modprobe "$(cat "$seed/module-name")"
    [[ -c /dev/kvm ]]
    mountpoint -q /run || mount -t tmpfs -o mode=0755 tmpfs /run
    echo "FIXTURE_PHASE=$phase" > /run/worker-host-fixture.env
    exit 0
fi
[[ $1 == run && $(cat /proc/1/comm) == systemd && -c /dev/kvm ]]
python3 -Bu "$seed/worker_host_restart_kvm.py" --guest "$phase"
