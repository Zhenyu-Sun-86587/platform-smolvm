#!/usr/bin/env bash
# Runs inside the disposable QEMU machine created by release_host_reboot.py.
set -Eeuo pipefail
umask 077
grep -qw 'sandbox.release_fixture=isolated' /proc/cmdline
seed=/mnt/release-fixture
[[ $(findmnt -n -o SOURCE -T "$seed") == /dev/vdb ]]
work=/var/lib/release-fixture
config=/etc/sandbox-platform/config.yaml
phase=$2
[[ $phase == upgraded || $phase == rebooted ]]

failed() {
    local rc=$?
    set +e
    if [[ $1 == run ]]; then
        journalctl -b -u platformd -u smolvm-serve --no-pager -n 35
    fi
    echo "RELEASE_FIXTURE_FAILURE phase=$phase line=$2 rc=$rc"
    exit "$rc"
}
trap 'failed "$1" "$LINENO"' ERR

if [[ $1 == bootstrap ]]; then
    if [[ $phase == upgraded ]]; then
        resize2fs /dev/vda
        install -d "/lib/modules/$(uname -r)/extra" "$work"
        install -m 0644 "$seed"/modules/*.ko "/lib/modules/$(uname -r)/extra/"
        depmod -a
        cat > /etc/systemd/system/release-fixture.service <<'UNIT'
[Unit]
Description=Exercise the installed platform releases in the disposable Worker host
After=local-fs.target network.target

[Service]
Type=simple
ExecStart=/bin/bash /mnt/release-fixture/guest.sh run ${FIXTURE_PHASE}
EnvironmentFile=/run/release-fixture.env
StandardOutput=tty
StandardError=tty
TTYPath=/dev/ttyS0

[Install]
WantedBy=multi-user.target
UNIT
        ln -s ../release-fixture.service /etc/systemd/system/multi-user.target.wants/release-fixture.service
    fi
    modprobe "$(cat "$seed/module-name")"
    [[ -c /dev/kvm ]]
    # systemd initializes /run as a tmpfs at PID 1 startup. Mount it first so
    # this boot's phase survives that handoff and never enters the disk image.
    mountpoint -q /run || mount -t tmpfs -o mode=0755 tmpfs /run
    echo "FIXTURE_PHASE=$phase" > /run/release-fixture.env
    exit 0
fi

[[ $1 == run && $(cat /proc/1/comm) == systemd && -c /dev/kvm ]]
ctl() { /usr/local/bin/sandboxctl --json admin --config "$config" "$@"; }
query() { sqlite3 -readonly /var/lib/sandbox-platform/platform.db "$1"; }
owner() {
    python3 - "$work/password" <<'PY'
import json, sys, time
from pathlib import Path
from urllib.error import URLError
from urllib.request import Request, urlopen
origin = "http://127.0.0.1:8443"
body = json.dumps({"username": "owner", "password": Path(sys.argv[1]).read_text()}).encode()
request = Request(origin + "/v1/auth/login", body, {"Content-Type": "application/json", "Origin": origin})
deadline = time.monotonic() + 20
while True:
    try:
        with urlopen(request, timeout=2) as result:
            print(json.load(result)["principal"]["principal_id"])
        break
    except URLError as error:
        if not isinstance(error.reason, ConnectionRefusedError) or time.monotonic() >= deadline:
            raise
        time.sleep(0.1)
PY
}
assert_services() {
    systemctl is-active --quiet smolvm-serve platformd smolvm-status.socket
    local platform_pid serve_pid
    platform_pid=$(systemctl show -p MainPID --value platformd)
    serve_pid=$(systemctl show -p MainPID --value smolvm-serve)
    [[ $(readlink -f "/proc/$platform_pid/exe") == "$(readlink -f /opt/sandbox-platform/current/platformd)" ]]
    [[ $(stat -c %U "/proc/$platform_pid") == sandbox-platform ]]
    echo "Installed units active: platformd PID $platform_pid, official Serve PID $serve_pid"
}
old_version=$(jq -er .version "$seed/old/release-manifest.json")
new_version=$(jq -er .version "$seed/new/release-manifest.json")
new_schema=$(jq -er .schema_version "$seed/new/release-manifest.json")
if [[ $phase == upgraded ]]; then
    head -c 32 /dev/urandom | base64 -w0 > "$work/password"
    bash "$seed/old/install.sh" --manifest "$seed/old/release-manifest.json" \
        --runtime-archive "$seed/runtime.tar.gz" --config "$seed/old/config.example.yaml" \
        --owner-password-file "$work/password"
    assert_services
    [[ $(query 'PRAGMA user_version') == 2 ]]
    owner > "$work/original-owner"
    query 'SELECT installation_id,control_epoch FROM meta' > "$work/identity"
    stat -c %i /var/lib/sandbox-platform/platform.db > "$work/db-inode"
    echo "Authenticated original owner under $old_version, schema 2"
    ctl upgrade plan --manifest "$seed/new/release-manifest.json"
    systemctl stop platformd
    ctl maintenance enter --reason release-upgrade
    ctl backup create --output "$work/old-backup" > "$work/old-receipt"
    ctl backup verify --archive "$work/old-backup" | cmp -s - "$work/old-receipt"
    ctl upgrade apply --manifest "$seed/new/release-manifest.json" --backup "$work/old-backup"
    assert_services
    systemctl stop platformd
    ctl backup create --output "$work/new-backup" > "$work/new-receipt"
    if ctl upgrade rollback --to "$old_version" --backup "$work/new-backup" > "$work/rollback.log" 2>&1; then
        echo "cross-schema rollback unexpectedly succeeded" >&2
        exit 1
    fi
    grep -F 'Rollback would cross registry schema versions' "$work/rollback.log"
    ctl maintenance exit
    systemctl start platformd
else
    # Both services are enabled by the stock installer and start at boot.
    timeout 40 bash -c 'until systemctl is-active --quiet smolvm-serve platformd smolvm-status.socket; do sleep 0.2; done'
fi
assert_services
[[ $(basename "$(readlink -f /opt/sandbox-platform/current)") == "$new_version" ]]
[[ $(query 'PRAGMA user_version') == "$new_schema" ]]
[[ $(query 'SELECT installation_id,control_epoch FROM meta') == "$(cat "$work/identity")" ]]
[[ $(stat -c %i /var/lib/sandbox-platform/platform.db) == "$(cat "$work/db-inode")" ]]
[[ $(owner) == "$(cat "$work/original-owner")" ]]
ctl backup verify --archive "$work/old-backup" | cmp -s - "$work/old-receipt"
ctl backup verify --archive "$work/new-backup" | cmp -s - "$work/new-receipt"
echo "$new_version schema $new_schema, original database inode, installation, control epoch, owner and both backup manifests verified"
sync
echo "RELEASE_FIXTURE_OK $phase $(cat /proc/sys/kernel/random/boot_id)"
