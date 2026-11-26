"""Public file and capture regressions against the System KVM fixture."""

from contextlib import closing
from hashlib import sha256
import json
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
    listing = platform.app(path + "/files/list?" + urlencode({"path": "/root"}))["items"]
    entry, = [item for item in listing if item["name"] == target.rsplit("/", 1)[1]]
    assert entry["type"] == "file" and entry["size"] == len(data), entry
    replacement = bytes.fromhex("00ff0a41")
    upload(replacement, "polish-replacement")
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
