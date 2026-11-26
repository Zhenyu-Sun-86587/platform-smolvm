"""Public file and capture regressions against the System KVM fixture."""

from contextlib import closing
from hashlib import sha256
import json
import sqlite3
from urllib.parse import urlencode

from managed_system_vsock import wait_for


def file_request(platform, method, route, data=None, key=None, length=None):
    headers = {"Origin": platform.origin, "Cookie": platform.cookie, "X-CSRF-Token": platform.csrf}
    if key:
        headers.update({"If-Control-Epoch": platform.epoch, "Idempotency-Key": key,
                        "Content-Type": "application/octet-stream"})
    if length is not None:
        headers["Content-Length"] = str(length)
    with closing(platform.api_connection(timeout=120)) as client:
        client.request(method, "/v1/" + route, body=data, headers=headers)
        with client.getresponse() as reply:
            return reply.status, reply.getheader("Content-Type", ""), reply.read()


def check_files(platform, path, ref):
    capability = platform.wait_capability(path, "files.write")["capabilities"]["files.write"]
    assert capability["constraints"]["transport"] == "native_agent"
    assert capability["constraints"]["max_bytes"] == 64 << 20
    target = "/root/polish-transfer.bin"
    route = path + "/files?" + urlencode({"path": target})
    data = bytes(range(256)) * (64 * 1024 * 1024 // 256)
    digest = sha256(data).hexdigest()

    def upload(payload, key):
        status, _, raw = file_request(platform, "PUT", route, payload, key)
        assert status == 200, (status, raw[:500])
        receipt = json.loads(raw)
        assert receipt["state"] == "uploaded" and receipt["size"] == len(payload)
        assert receipt["sha256"] == sha256(payload).hexdigest() and receipt["path"] == target
        return receipt

    def download():
        status, kind, raw = file_request(platform, "GET", route)
        assert (status, kind) == (200, "application/octet-stream"), (status, kind, raw[:200])
        return raw

    receipt = upload(data, "polish-upload-original")
    assert platform.guest(ref, ["sha256sum", target]).decode().split()[0] == digest
    assert sha256(download()).hexdigest() == digest
    assert platform.guest(ref, ["stat", "-c", "%a:%u:%g", target]).strip() == b"600:0:0"
    platform.guest(ref, ["chown", "1000:1001", target])
    platform.guest(ref, ["chmod", "0640", target])
    listing = platform.app(path + "/files/list?" + urlencode({"path": "/root"}))["items"]
    entry, = [item for item in listing if item["name"] == target.rsplit("/", 1)[1]]
    assert entry["type"] == "file" and entry["size"] == len(data), entry
    replacement = bytes.fromhex("00ff0a41")
    upload(replacement, "polish-replacement")
    assert platform.guest(ref, ["stat", "-c", "%a:%u:%g", target]).strip() == b"640:1000:1001"
    assert upload(data, "polish-upload-original") == receipt
    assert download() == replacement, "replaying a completed intent overwrote a later write"
    status, _, raw = file_request(platform, "PUT", route, replacement, "polish-upload-original")
    assert status == 409 and json.loads(raw)["error"]["code"] == "IDEMPOTENCY_CONFLICT", (status, raw)
    status, _, raw = file_request(platform, "PUT", route, b"", "polish-oversize", len(data) + 1)
    assert status >= 400 and json.loads(raw)["error"]["code"] == "FILE_TOO_LARGE", (status, raw)
    # Close a real ingress connection before the body completes.
    with closing(platform.api_connection()) as client:
        client.request("PUT", "/v1/" + route, b"partial", {"Content-Length": "1024",
            "Origin": platform.origin, "Cookie": platform.cookie, "X-CSRF-Token": platform.csrf,
            "Content-Type": "application/octet-stream", "If-Control-Epoch": platform.epoch,
            "Idempotency-Key": "polish-disconnected"})
    assert download() == replacement
    # Fail only persistence of the real post-rename receipt, in this fixture's own DB.
    key = "polish-lost-receipt"
    with closing(sqlite3.connect(platform.root / "platform/platform.db", timeout=5)) as db:
        db.execute("""CREATE TRIGGER polish_receipt_fault BEFORE UPDATE OF response_json ON idempotency
                   WHEN OLD.key = 'polish-lost-receipt'
                   BEGIN SELECT RAISE(ABORT, 'injected receipt loss'); END""")
        try:
            status, _, raw = file_request(platform, "PUT", route, b"committed", key)
            error = json.loads(raw)["error"]
            assert status == 409 and error["code"] == "OUTCOME_UNKNOWN" and error["effect"] == "unknown", error
            assert error["retry"] == "verify_original_intent" and download() == b"committed"
        finally:
            db.execute("DROP TRIGGER polish_receipt_fault")
    upload(replacement, "polish-after-unknown")
    status, _, raw = file_request(platform, "PUT", route, b"committed", key)
    assert status == 409 and json.loads(raw)["error"]["effect"] == "unknown"
    assert download() == replacement, "a retry of an uncertain upload rewrote the path"
    print(json.dumps(dict(case="files", bundle=platform.bundle_id, bytes=len(data), sha256=digest,
                          upload_id=receipt["upload_id"], execution=ref["incarnation_id"],
                          replay_preserved_later_bytes=replacement.hex())), flush=True)


def check_capture(platform, path, ref):
    before = platform.app(path + "/resources")["requested"]
    platform.guest(ref, ["sh", "-ec", "printf captured >/root/polish-marker; sync /root/polish-marker"])
    saved = platform.complete(platform.change(path + "/snapshots", "polish-checkpoint",
                              {"capture_mode": "vm", "label": "Before resize"},
                              platform.wait_capability(path, "state.checkpoint")))
    snapshot_id = saved["result"]["snapshot_id"]
    item_path = "snapshots/" + snapshot_id
    summary = platform.app(item_path)["capture_summary"]
    assert summary["configuration"]["status"] == "verified", summary
    assert summary["configuration"]["spec"]["resources"] == before, (before, summary)
    assert summary["ram"] == "included" and any(d["role"] == "root" for d in summary["disks"]), summary
    platform.complete(platform.change(path, "polish-limit-parent",
                                      {"resources": {"cpu_limit_millicpus": 750}},
                                      platform.app(path), method="PATCH"))
    assert platform.app(path + "/resources")["requested"]["cpu_limit_millicpus"] == 750
    platform.guest(ref, ["sh", "-ec", "printf later > /root/polish-marker"])
    assert platform.app(item_path)["capture_summary"] == summary
    restored = platform.complete(platform.change("restores", "polish-restore", {
        "snapshot_id": snapshot_id, "name": "polish-restore", "volume_restore_policy": "clone_conflicts"}))
    mid = restored["machine_id"]
    _, child = platform.context(mid)
    assert child["incarnation_id"] != ref["incarnation_id"]
    actual = platform.app("machines/" + mid + "/resources")
    assert actual["requested"] == before, (before, actual)
    wait_for(lambda: platform.guest(child, ["cat", "/root/polish-marker"]) == b"captured",
             retry=(RuntimeError, OSError))
    platform.guest(child, ["sh", "-ec", "printf independent >/root/polish-marker"])
    assert platform.guest(ref, ["cat", "/root/polish-marker"]) == b"later"
    print(json.dumps(dict(case="capture", bundle=platform.bundle_id, snapshot_id=snapshot_id,
                          captured=before, parent_limit_after=750, restored_request=actual["requested"],
                          restored_execution=child["incarnation_id"], summary=summary,
                          restored_cpu_limit=actual.get("cpu_limit"))), flush=True)


def check_lease_and_fork(platform, path, ref):
    import time
    from authorized_services_kvm import worker_get

    lease = platform.app(path + "/leases", "POST", {"reason": "polish slow HTTP", "ttl_seconds": 180,
        "semantics_version": 2}, {"If-Control-Epoch": platform.epoch, "Idempotency-Key": "polish-run-lease",
                                 "If-Match": f'"{platform.app(path)["revision"]}"'})
    assert lease["state"] == "active" and lease["execution_epoch"] == ref["execution_epoch"]
    # Both endpoints live in the test guest. Only the fixture releases the model response.
    program = r"""
use strict;
use warnings;
use IO::Socket::INET;
my $server = IO::Socket::INET->new(LocalAddr => '127.0.0.1', LocalPort => 0,
                                    Listen => 1, Proto => 'tcp') or die $!;
my $port = $server->sockport;
my $pid = fork(); defined $pid or die $!;
if ($pid == 0) {
    close $server;
    my $client = IO::Socket::INET->new(PeerAddr => '127.0.0.1', PeerPort => $port, Proto => 'tcp') or die $!;
    print {$client} "GET /model HTTP/1.0\r\n\r\n";
    local $/;
    my $response = <$client>;
    open my $out, '>', '/run/polish-model-result' or die $!;
    print {$out} $response;
    exit;
}
my $conn = $server->accept or die $!;
my $request = <$conn>;
die "unexpected request" unless $request eq "GET /model HTTP/1.0\r\n";
while (<$conn>) { last if $_ eq "\r\n"; }
open my $marker, '>', '/run/polish-model-wait' or die $!;
print {$marker} $pid; close $marker;
until (-e '/run/polish-model-release') { select undef, undef, undef, 0.2; }
print {$conn} "HTTP/1.0 200 OK\r\nContent-Length: 2\r\n\r\nOK";
close $conn;
waitpid $pid, 0;
"""
    platform.guest(ref, ["sh", "-ec", "cat >/run/polish-model.pl; perl /run/polish-model.pl </dev/null >/run/polish-model.log 2>&1 &"], stdin=program)
    wait_for(lambda: platform.guest(ref, ["cat", "/run/polish-model-wait"]).strip(), retry=(RuntimeError,))
    policy = platform.app(path + "/lifecycle")
    platform.complete(platform.change(path, "polish-short-idle", {
        "expected_policy_revision": policy["desired_revision"],
        "lifecycle_policy": {"auto_standby": True, "standby_after_seconds": 2, "auto_hibernate": False,
                             "min_running_after_wake_seconds": 0, "keep_running": False}
    }, platform.app(path), method="PATCH"))
    start = time.monotonic()
    while time.monotonic() - start < 4:
        status = worker_get(platform, "GetLifecycleStatus", ref)
        assert status["power"] == "RUNNING" and "lease:polish slow HTTP" in status["blockers"], status
        time.sleep(.25)
    view = platform.wait_capability(path, "state.fork_batch")
    batch = platform.change(path + "/fork-batches", "polish-manual-fork", {
        "children": [{"name": "polish protected child"}], "require_independent": True}, view)
    child, = platform.complete(batch)["children"]
    assert child["state"] == "ready" and child["resource_independence"] == "verified", child
    platform.guest(ref, ["touch", "/run/polish-model-release"])
    wait_for(lambda: platform.guest(ref, ["cat", "/run/polish-model-result"]).endswith(b"\r\n\r\nOK"),
             retry=(RuntimeError,))
    print(json.dumps(dict(case="lease_wait_fork", operation=batch["operation_id"], child=child["machine_id"],
                          source=ref["incarnation_id"], lease_state=lease["state"], bundle=platform.bundle_id)), flush=True)


def check_passive_metrics(platform, path, ref):
    from pathlib import Path
    from datetime import datetime

    inventory = platform.native(f"/instances/{ref['instance_id']}/inventory")
    pid = inventory["execution"]["pid"]
    group, = [line.removeprefix("0::") for line in Path(f"/proc/{pid}/cgroup").read_text().splitlines()
              if line.startswith("0::")]
    scope = Path("/sys/fs/cgroup") / group.lstrip("/")

    def cpu_millis():
        values = dict(row.split() for row in (scope / "cpu.stat").read_text().splitlines())
        return int(values["usage_usec"]) // 1000

    low = cpu_millis()
    sample = platform.app(path + "/metrics")
    high = cpu_millis()
    assert sample["source"] == "cgroup_v2" and sample["scope"] == "machine" and sample["availability"] == "partial", sample
    assert low <= sample["cpu_millis"] <= high and sample["memory_mb"] > 0, (low, sample, high)
    assert sample.get("rss_mb") is None and datetime.fromisoformat(sample["observed_at"]).tzinfo
    assert sample["execution"]["incarnation_id"] == ref["incarnation_id"]
    system = platform.app("system/metrics")
    assert system["scope"] == "worker" and system["allocated_memory_mb"] > 0 and system.get("used_memory_mb") is None, system
    platform.complete(platform.change(path + "/standby", "polish-standby", {},
                                      platform.wait_capability(path, "lifecycle.standby")))
    for _ in range(3):
        platform.app("machines")
        assert platform.app(path)["power_state"] == "standby"
        metrics = platform.app(path + "/metrics")
        assert metrics["platform"]["active_access_holds"] == 0
        assert platform.native(f"/instances/{ref['instance_id']}/inventory")["inventory"]["source_disposition"] == "STANDBY"
    listing = platform.app(path + "/files/list?path=/root")
    assert isinstance(listing["items"], list)
    assert platform.native(f"/instances/{ref['instance_id']}/inventory")["inventory"]["source_disposition"] == "RUNNING"
    print(json.dumps(dict(case="metrics_standby", bundle=platform.bundle_id, execution=sample["execution"],
                          observed_at=sample["observed_at"], cpu_millis=sample["cpu_millis"],
                          cpu_bracket=[low, high], memory_mb=sample["memory_mb"],
                          worker_allocated_mb=system["allocated_memory_mb"], file_woke_guest=True)), flush=True)


def check_historical_capture(platform, path, ref, candidate):
    platform.guest(ref, ["sh", "-ec", "printf previous >/root/polish-old; sync /root/polish-old"])
    saved = platform.complete(platform.change(path + "/snapshots", "previous-checkpoint",
                              {"capture_mode": "vm", "label": "Historical point"},
                              platform.wait_capability(path, "state.checkpoint")))
    snapshot = saved["result"]["snapshot_id"]
    assert "capture_summary" not in platform.app("snapshots/" + snapshot)
    control = platform.epoch
    platform.platform_command[0] = candidate
    platform.restart()
    assert platform.app("control")["control_epoch"] == control
    metadata = platform.app("snapshots/" + snapshot)
    assert metadata["capture_summary"]["configuration"]["status"] == "unknown", metadata
    assert metadata["capture_summary"]["ram"] == "unknown"
    restored = platform.complete(platform.change("restores", "current-historical-restore", {
        "snapshot_id": snapshot, "name": "historical restore", "volume_restore_policy": "clone_conflicts"}))
    child = restored["machine_id"]
    _, execution = platform.context(child)
    assert execution["incarnation_id"] != ref["incarnation_id"]
    actual = platform.app("machines/" + child + "/resources")
    assert actual["requested"] == {}, actual
    wait_for(lambda: platform.guest(execution, ["cat", "/root/polish-old"]) == b"previous",
             retry=(RuntimeError, OSError))
    print(json.dumps(dict(case="historical_reader", bundle=platform.bundle_id, snapshot=snapshot,
                          summary=metadata["capture_summary"], restored_execution=execution["incarnation_id"],
                          operation=restored["operation_id"], restored_resources=actual)), flush=True)
