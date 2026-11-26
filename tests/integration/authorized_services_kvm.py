#!/usr/bin/env python3
"""Exercise ordinary authorized service access and owned lifecycle operations on KVM."""

import argparse
import base64
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack
import json
import os
from pathlib import Path
import shlex
import select
import socket
import sqlite3
import ssl
import subprocess
import sys
import tempfile
from threading import Barrier

from managed_system_vsock import Platform, wait_for, worker


DIRECT = {"private-tcp", "private-udp"}
OWNER = {"system-tasks-delete", "system-rollback"}
SYSTEM = {"system-boot", "system-fork", "system-restore", "system-lifecycle"} | OWNER
NETWORK = dict(network_id="owned-services", route_domain_id="owned-services", cidr="10.44.61.0/24")
PROGRAM = "/run/authorized-service.pl"
RECEIPTS = "/run/authorized-service-receipts"
SERVICE_PORT = 18081
HTTP_WAKE_WAITERS = 2


def identity(ref):
    return {key: ref[key] for key in ("instance_id", "incarnation_id", "execution_epoch")}


def witness(platform, case, **facts):
    print("Authorized KVM", json.dumps(dict(case=case, bundle_digest=platform.bundle_id, **facts), sort_keys=True), flush=True)


def machine(platform, label, mode=None, *, light=False):
    resources = dict(cpu_count=1, memory_mib=512) if mode or light else dict(cpu_count=2, memory_mib=768)
    parameters = dict(name=label, template_id="t_system", boot_mode="system", runtime_instance_id=platform.serve_id,
                      start=True, resources=resources)
    if mode:
        parameters["network"] = dict(network_id=NETWORK["network_id"], mode=mode)
    created = platform.change("machines", "owned-" + label, parameters)
    platform.complete(created)
    mid = created["machine_id"]
    view, ref = platform.context(mid)
    wait_for(lambda: platform.guest(ref, ["cat", "/proc/1/comm"]) == b"systemd\n",
             retry=(RuntimeError, OSError))
    assert view["power_state"] == "running" and ref["execution_epoch"] == 1, (view, ref)
    return mid, view, ref


def worker_get(platform, method, ref, payload=None):
    return worker(platform.worker, method, payload, route_id="deep", bundle_digest=platform.bundle_id,
                  expected=identity(ref))


def policy_standby(platform, mid, ref, confirmed, marker, volatile_path):
    path = f"machines/{mid}"
    lifecycle = path + "/lifecycle"
    def policy_ready():
        status = platform.app(lifecycle)
        return status if status["supported"] and status["applied_revision"] == status["desired_revision"] else None

    wait_for(policy_ready, timeout=25)

    def protect(key, value):
        status = platform.app(lifecycle)
        policy = {"keep_running": value, "auto_standby": False, "auto_hibernate": False}
        configured = platform.change(path, key, {
            "expected_policy_revision": status["desired_revision"], "lifecycle_policy": policy,
        }, platform.app(path), method="PATCH")
        platform.complete(configured)
        applied = platform.app(lifecycle)
        current = worker_get(platform, "GetLifecycleStatus", ref, {})
        assert identity(current) == identity(ref) and current["power"] == "RUNNING", current
        assert applied["applied_revision"] == current["applied_revision"] == status["desired_revision"] + 1
        assert applied["applied"]["keep_running"] == current["effective_policy"]["keep_running"] == value
        return applied["applied_revision"]

    protected = protect("owned-enable-running-protection", True)
    cleared = protect("owned-clear-running-protection", False)
    events = worker_get(platform, "QueryLifecycleEvents", ref, {"after": 0})["events"]
    cursor = max((event["seq"] for event in events), default=0)
    view = platform.wait_capability(path, "lifecycle.standby")
    platform.complete(platform.change(path + "/standby", "owned-standby", {}, view))
    parked = worker_get(platform, "GetLifecycleStatus", ref, {})
    inventory = platform.native(f"/instances/{ref['instance_id']}/inventory")
    assert parked["power"] == "STANDBY" and inventory["inventory"]["source_disposition"] == "STANDBY", inventory
    assert identity(parked) == identity(ref)

    def lease(route, method, key, body, revision):
        return platform.app(route, method, body, {"If-Control-Epoch": platform.epoch,
                            "If-Match": f'"{revision}"', "Idempotency-Key": key}, timeout=60)

    hold = lease(path + "/leases", "POST", "owned-wake-via-lease", {
        "reason": "Authorized standby resume", "ttl_seconds": 60, "semantics_version": 2,
    }, platform.app(path)["revision"])
    assert hold["state"] == "active" and hold["execution_epoch"] == ref["execution_epoch"], hold["state"]
    awake = worker_get(platform, "GetLifecycleStatus", ref, {})
    assert awake["power"] == "RUNNING" and not awake["controlled_suspended"] and identity(awake) == identity(ref), awake
    confirmed(ref)

    def completed_wake():
        new = worker_get(platform, "QueryLifecycleEvents", ref, {"after": cursor})["events"]
        operations = {event["operation_id"] for event in new if event["origin"] == "wake"}
        assert len(operations) <= 1, new
        if operations and any(event["origin"] == "wake" and event["stage"] == "completed" for event in new):
            return operations.pop()

    wake = wait_for(completed_wake, timeout=10)
    released = lease("leases/" + hold["lease_id"], "DELETE", "owned-release-resume-lease",
                     {"lease_token": hold["lease_token"]}, hold["revision"])
    assert released["state"] == "released", released
    durable_path = "/root/authorized-lifecycle-state"
    platform.guest(ref, ["/bin/sh", "-ec", 'cat > "$1"; sync "$1"', "owned", durable_path], stdin=marker)
    view = platform.wait_capability(path, "lifecycle.stop")
    stopped = platform.complete(platform.change(path + "/stop", "owned-cold-stop", {}, view))
    stopped_view = platform.app(path)
    no_incarnation = {**ref, "incarnation_id": None}
    offline = worker_get(platform, "GetLifecycleStatus", no_incarnation, {})
    assert stopped_view["power_state"] == "stopped" and offline["power"] == "STOPPED", offline
    assert offline["execution_epoch"] == ref["execution_epoch"] and not offline.get("incarnation_id"), offline
    started = platform.complete(platform.change(path + "/start", "owned-cold-restart", {}, stopped_view))
    _, restarted = platform.context(mid)
    assert restarted["instance_id"] == ref["instance_id"] and restarted["incarnation_id"] != ref["incarnation_id"]
    assert restarted["execution_epoch"] == ref["execution_epoch"] + 1, restarted
    assert wait_for(lambda: platform.guest(restarted, ["/bin/sh", "-ec", 'test ! -e "$2"; cat "$1"',
                                                       "owned", durable_path, volatile_path]) == marker.encode(),
                    retry=(RuntimeError, OSError))
    witness(platform, "system-lifecycle", execution=identity(ref), standby=identity(parked),
            resumed=identity(awake), protected_revision=protected, cleared_revision=cleared,
            wake_operation_id=wake, released_lease_id=hold["lease_id"], cold_started=identity(restarted),
            stop_operation_id=stopped["operation_id"], start_operation_id=started["operation_id"])


def owner_tasks_delete(platform, mid, ref):
    path = f"machines/{mid}"
    marker = "task-" + os.urandom(12).hex()

    def task_ready():
        view = platform.app(path)
        return view if view.get("task_metadata_status") == "complete" else None

    view = wait_for(task_ready, timeout=30)
    saved = platform.complete(platform.change(path + "/hibernate", "owned-before-task", {}, view))
    capture = saved["result"]["statepoint"]["capture_id"]
    paused = platform.app(path)
    assert paused["power_state"] == "hibernated"
    task = {"argv": ["/bin/sh", "-ec", 'printf "%s\\n" "$1"; exec sleep 120', "owned", marker], "task_key": "owned-task"}
    accepted = platform.change(path + "/tasks", "owned-start-task", task, paused)
    started = platform.complete(accepted)
    assert platform.change(path + "/tasks", "owned-start-task", task, paused)["operation_id"] == accepted["operation_id"]
    _, active = platform.context(mid)
    assert active["instance_id"] == ref["instance_id"] and active["incarnation_id"] != ref["incarnation_id"]
    assert active["execution_epoch"] == ref["execution_epoch"] + 1, active
    tid = started["result"]["task_id"]
    task_path = "tasks/" + tid
    wait_for(lambda: base64.b64decode(platform.app(task_path + "/output")["data_base64"]) == (marker + "\n").encode())
    live = platform.app(task_path)
    assert live["state"] == "running" and live["runner"]["pid"] > 1, live
    now = platform.app(path)
    cancelled = platform.complete(platform.app(task_path + "/cancel", "POST", {"grace_seconds": 1}, {
        "If-Control-Epoch": platform.epoch, "Idempotency-Key": "owned-cancel-task", "If-Match": f'"{now["revision"]}"',
    }, 202))
    assert cancelled["result"]["state"] == platform.app(task_path)["state"] == "cancelled", cancelled
    view = platform.wait_capability(path, "lifecycle.stop")
    stopped = platform.complete(platform.change(path + "/stop", "owned-stop-for-delete", {}, view))
    assert stopped["source_disposition"] == "EXITED", stopped
    stopped_view = platform.app(path)
    receipt = platform.complete(platform.change(path, "owned-delete", {}, stopped_view, method="DELETE"))
    assert receipt["source_disposition"] == "EXITED" and receipt["result"]["execution_epoch"] == active["execution_epoch"]
    assert platform.change(path, "owned-delete", {}, stopped_view, method="DELETE")["operation_id"] == receipt["operation_id"]
    tombstone = platform.app(path)
    assert tombstone["desired_power"] == "deleted" and tombstone["deleted_at"] and not tombstone.get("binding"), tombstone
    with sqlite3.connect(f"file:{platform.root / 'catalog/catalog.sqlite'}?mode=ro", uri=True) as catalog:
        live_slots = catalog.execute("SELECT count(*) FROM instance_bindings WHERE instance_id=?", (active["instance_id"],)).fetchone()
        assert live_slots == (0,), live_slots
    witness(platform, "system-tasks-delete", original=identity(ref), execution=identity(active), task_id=tid, capture_id=capture,
            task_operation_id=started["operation_id"], cancel_operation_id=cancelled["operation_id"],
            stop_operation_id=stopped["operation_id"], delete_operation_id=receipt["operation_id"], marker=marker)


def owner_rollback(platform, mid, ref):
    path = f"machines/{mid}"
    ram, disk = "/run/owned-rollback", "/root/owned-rollback"
    original = "before-" + os.urandom(12).hex()
    changed = "after-" + os.urandom(12).hex()

    def write(marker):
        platform.guest(ref, ["/bin/sh", "-ec", 'printf %s "$1" >"$2"; printf %s "$1" >"$3"; sync "$3"',
                             "owned", marker, ram, disk])

    write(original)
    saved = platform.complete(platform.change(path + "/snapshots", "owned-rollback-base", {"capture_mode": "vm"},
                                               platform.wait_capability(path, "state.checkpoint")))["result"]
    assert saved["statepoint"]["phase"] == "durable", saved
    write(changed)
    assert platform.guest(ref, ["cat", ram, disk]) == (changed * 2).encode()
    old = platform.native(f"/instances/{ref['instance_id']}/inventory")["execution"]
    assert identity(old) == identity(ref)
    with os.fdopen(os.pidfd_open(old["pid"]), "rb", buffering=0) as pidfd:
        assert not select.select([pidfd], [], [], 0)[0]
        rollback = platform.complete(platform.change(path + "/rollback", "owned-rollback", {
            "snapshot_id": saved["snapshot_id"], "volume_restore_policy": "clone_conflicts",
        }, platform.wait_capability(path, "state.rollback")))
        assert select.select([pidfd], [], [], 5)[0], "rollback left the original VMM live"
    _, active = platform.context(mid)
    assert active["instance_id"] == ref["instance_id"] and active["incarnation_id"] != ref["incarnation_id"]
    assert active["execution_epoch"] == ref["execution_epoch"] + 1, active
    assert wait_for(lambda: platform.guest(active, ["cat", ram, disk]) == (original * 2).encode(), retry=(RuntimeError, OSError))
    rescue = rollback["result"]["rescue_capture_id"]
    assert rescue and rescue != saved["statepoint"]["capture_id"], rollback
    witness(platform, "system-rollback", original=identity(ref), restored=identity(active), snapshot_id=saved["snapshot_id"],
            operation_id=rollback["operation_id"], rescue_capture_id=rescue, marker=original)


def system_lifecycle(platform, case):
    mid, _, ref = machine(platform, "system", light=case == "system-lifecycle" or case in OWNER)
    if case == "system-tasks-delete":
        owner_tasks_delete(platform, mid, ref)
        return
    if case == "system-rollback":
        owner_rollback(platform, mid, ref)
        return
    machine_path = f"machines/{mid}"
    marker = "ram-" + os.urandom(12).hex()
    path = "/run/authorized-state"
    platform.guest(ref, ["/bin/sh", "-ec", 'cat > "$1"', "owned", path], stdin=marker)

    def confirmed(ref):
        assert wait_for(lambda: platform.guest(ref, ["cat", path]) == marker.encode(),
                        retry=(RuntimeError, OSError))

    confirmed(ref)
    if case == "system-boot":
        witness(platform, case, execution=identity(ref), marker=marker)
        return
    if case == "system-lifecycle":
        policy_standby(platform, mid, ref, confirmed, marker, path)
        return
    if case == "system-fork":
        fork_view = platform.wait_capability(machine_path, "state.fork_batch")
        fork = platform.complete(platform.change(machine_path + "/fork-batches", "owned-fork", {
            "children": [{"name": "owned child A"}, {"name": "owned child B"}], "require_independent": True,
        }, fork_view))
        assert fork["source_lock_released"] and fork["source_disposition"] == "RUNNING", fork
        children = fork["children"]
        assert len(children) == 2 and all(child["state"] == "ready" and child["resource_independence"] == "verified"
                                           for child in children), children
        refs = [platform.context(child["machine_id"])[1] for child in children]
        assert len({ref["incarnation_id"], *(child["incarnation_id"] for child in refs)}) == 3
        for descendant in refs:
            confirmed(descendant)
        confirmed(ref)
        witness(platform, case, source=identity(ref), children=[identity(child) for child in refs], marker=marker)
        return

    saved = platform.complete(platform.change(machine_path + "/snapshots", "owned-checkpoint", {
        "capture_mode": "vm", "label": "Authorized checkpoint",
    }, platform.wait_capability(machine_path, "state.checkpoint")))["result"]
    restored = platform.complete(platform.change("restores", "owned-restore", {
        "snapshot_id": saved["snapshot_id"], "name": "owned restore", "volume_restore_policy": "clone_conflicts",
    }))
    restored_id = restored["machine_id"]
    reopened = platform.context(restored_id)[1]
    assert reopened["instance_id"] != ref["instance_id"] and reopened["incarnation_id"] != ref["incarnation_id"]
    confirmed(reopened)
    sleeping = platform.complete(platform.change(machine_path + "/hibernate", "owned-system-hibernate", {},
                                                  platform.app(machine_path)))
    capture = sleeping["result"]["statepoint"]["capture_id"]
    started = platform.complete(platform.change(machine_path + "/start", "owned-system-wake", {},
                                                 platform.app(machine_path)))
    live = platform.context(mid)[1]
    assert live["execution_epoch"] == ref["execution_epoch"] + 1
    assert started["result"]["statepoint"]["capture_id"] == capture
    confirmed(live)
    witness(platform, case, snapshot_id=saved["snapshot_id"], restored=identity(reopened),
            capture_id=capture, original=identity(ref), saved_execution=identity(live), marker=marker)


def write_service(platform, execution):
    body = (Path(__file__).resolve().parents[1] / "fixtures/authorized-service.pl").read_text()
    platform.guest(execution, ["/bin/sh", "-ec", 'cat > "$1"', "owned", PROGRAM], stdin=body)


def create_service(platform, mid, protocol, port):
    path = f"machines/{mid}"
    done = platform.complete(platform.change(path + "/services", "owned-service", {
        "name": protocol, "protocol": protocol, "guest_port": port,
    }, platform.app(path)))
    return done["result"]["service_id"]


def tls_setup(root):
    keys = root / "keys"
    keys.mkdir(mode=0o700)

    def openssl(*arguments):
        subprocess.run(["openssl", *arguments], cwd=keys, check=True,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    openssl("req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "2", "-subj", "/CN=Owned KVM fixture CA",
            "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign",
            "-keyout", "ca.key", "-out", "ca.pem")
    openssl("req", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=Owned KVM fixture",
            "-keyout", "server.key", "-out", "server.csr")
    (keys / "server.ext").write_text("subjectAltName=IP:127.0.0.1,DNS:*.owned-services.test\nextendedKeyUsage=serverAuth\n")
    openssl("x509", "-req", "-in", "server.csr", "-CA", "ca.pem", "-CAkey", "ca.key", "-CAcreateserial",
            "-days", "1", "-extfile", "server.ext", "-out", "server.pem")
    openssl("req", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=Owned scoped client",
            "-keyout", "client.key", "-out", "client.csr")
    ca = keys / "ca.pem"
    return keys, (keys / "server.pem", keys / "server.key", ssl.create_default_context(cafile=str(ca))), {
        "apps_domain": "owned-services.test", "client_ca_file": str(ca), "client_ca_key_file": str(keys / "ca.key"),
    }


def prepare_openssh(platform, keys):
    key = keys / "ssh"
    subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key], check=True)
    platform.change("ssh-keys", "owned-public-key", {"label": "Owned KVM public key",
                    "public_key": key.with_suffix(".pub").read_text()}, status=200)
    return key


def token(platform, mid):
    return platform.change("tokens", "owned-tunnel-token", {"label": "Owned service access",
                           "scope": {"access": "tunnel", "machine_id": mid}}, status=200)["token"]


def service_client(platform, mid, case, sid, keys, ssh_key, sandboxctl):
    service = platform.app(f"services/{sid}")
    if case == "http":
        credential = token(platform, mid)

        def http(request, *, ready=None):
            with ExitStack() as clients:
                connection = platform.api_connection(timeout=65)
                clients.callback(connection.close)
                if ready is not None:
                    connection.connect()
                    ready.wait()
                connection.request("POST", "/receipt/" + request, body=b"", headers={
                    "Host": service["public_host"], "Authorization": f"Bearer {credential}",
                    "Content-Length": "0",
                })
                reply = connection.getresponse()
                return reply.status, reply.getheader("X-Sandbox-Forwarding"), reply.read()

        return http
    if case == "tcp-mtls":
        certificate = platform.change("client-certificates", "owned-mtls-certificate", {
            "csr": (keys / "client.csr").read_text(), "scope": {"service_ids": [sid]}, "ttl_seconds": 600,
        }, status=200)
        cert_path = keys / "client.pem"
        cert_path.write_text(certificate["certificate_pem"])
        context = ssl.create_default_context(cafile=str(keys / "ca.pem"))
        context.load_cert_chain(cert_path, keys / "client.key")
        port = service["public_port"]
        wait_for(lambda: f":{port} " in subprocess.check_output(["ss", "-ltn"], text=True))

        def tcp(request):
            with socket.create_connection(("127.0.0.1", port), timeout=65) as raw:
                with context.wrap_socket(raw, server_hostname="127.0.0.1") as socket_tls:
                    socket_tls.sendall(request.encode() + b"\n")
                    with socket_tls.makefile("rb") as receipt:
                        return receipt.readline()

        return tcp

    credential = token(platform, mid)
    key = wait_for(lambda: platform.app(f"services/{sid}").get("ssh_host_key"),
                   retry=(OSError, RuntimeError))
    known = keys / "known_hosts"
    hostname = "owned-" + mid
    known.write_text(hostname + " " + key + "\n")
    environment = {**os.environ, "SANDBOX_TOKEN": credential, "SANDBOX_URL": platform.origin,
                   "SANDBOX_CA_FILE": str(keys / "ca.pem")}
    proxy = f"{shlex.quote(str(sandboxctl))} ssh-proxy --machine {mid}"
    args = ["ssh", "-F", "/dev/null", "-i", ssh_key, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
            "-o", "StrictHostKeyChecking=yes", "-o", f"UserKnownHostsFile={known}",
            "-o", f"HostKeyAlias={hostname}", "-o", f"ProxyCommand={proxy}", "root@workspace"]

    def ssh(request):
        value = shlex.quote(request)
        remote = f"printf '%s\\n' {value} >> {RECEIPTS}; printf '%s\\n' {value}"
        return subprocess.check_output([*args, remote], env=environment, timeout=65)

    return ssh


def http_wake_admissions(requests, replies):
    accepted = []
    rejected = []
    for request, reply in zip(requests, replies, strict=True):
        status, stage, body = reply
        if status == 200:
            assert reply == (200, None, request.encode() + b"\n"), reply
            accepted.append(request)
        else:
            assert (status, stage) == (409, "not_started"), reply
            error = json.loads(body)["error"]
            assert (error["code"], error["message"], error["effect"]) == (
                "RESOURCE_EXHAUSTED", "too many concurrent access requests", "none"), error
            rejected.append(dict(request=request, status=status, stage=stage, code=error["code"], effect=error["effect"]))
    assert len(accepted) == HTTP_WAKE_WAITERS and len(rejected) == 1, (accepted, rejected)
    return accepted, rejected[0]


def service_access(platform, case, keys, ssh_key, sandboxctl, mode):
    mid, _, ref = machine(platform, "destination", mode)
    path = f"machines/{mid}"
    protocol = case.removeprefix("private-")
    if case != "ssh":
        write_service(platform, ref)
        service_kind = "tcp" if protocol == "tcp-mtls" else protocol
        platform.guest(ref, ["systemd-run", "--unit=owned-service", "/usr/bin/perl", PROGRAM, service_kind, str(SERVICE_PORT)])
        proc = "udp" if protocol == "udp" else "tcp"
        wait_for(lambda: f":{SERVICE_PORT:04X}".encode() in platform.guest(ref, ["cat", f"/proc/net/{proc}"]))
    if case in DIRECT:
        source_mode = "fixed_internal" if mode == "native" else "native"
        source, _, source_ref = machine(platform, "source", source_mode)
        write_service(platform, source_ref)
        endpoint = platform.app(path + "/network")
        route = f"{path}/network/wake-rules/{source}/{protocol}/{SERVICE_PORT}"
        rule = platform.change(route, "owned-packet-rule", dict(enabled=True, expected_revision=0),
                               platform.app(path), method="PUT", status=200)
        assert rule["enabled"] and rule["source_current"], rule
        address = endpoint["private_ipv4"]

        def invoke(request):
            return platform.guest(source_ref, ["/usr/bin/perl", PROGRAM, "send-" + protocol, str(SERVICE_PORT), address, request])
    else:
        sid = create_service(platform, mid, protocol, 2222 if case == "ssh" else SERVICE_PORT)
        invoke = service_client(platform, mid, case, sid, keys, ssh_key, sandboxctl)
    old_events = worker_get(platform, "QueryLifecycleEvents", ref, {"after": 0})["events"]
    cursor = max((event["seq"] for event in old_events), default=0)
    saved = platform.complete(platform.change(path + "/hibernate", "owned-service-hibernate", {}, platform.app(path)))
    capture = saved["result"]["statepoint"]["capture_id"]
    assert platform.app(path)["power_state"] == "hibernated"
    check_forwarding = platform.args.check_forwarding
    check_queue = check_forwarding and case == "http"
    request_count = HTTP_WAKE_WAITERS + 1 if check_queue else (1 if protocol == "udp" else 2)
    requests = [f"owned-{os.urandom(10).hex()}" for _ in range(request_count)]
    barrier = Barrier(len(requests), timeout=12)

    def first_request(request):
        if check_queue:
            return invoke(request, ready=barrier)
        barrier.wait()
        return invoke(request)

    with ThreadPoolExecutor(max_workers=len(requests)) as clients:
        replies = list(clients.map(first_request, requests))
    checks = {}
    if check_queue:
        requests, rejected = http_wake_admissions(requests, replies)
        checks["wake_queue"] = dict(limit=HTTP_WAKE_WAITERS, admitted=requests.copy(), rejected=rejected)
    elif case == "http":
        assert replies == [(200, None, request.encode() + b"\n") for request in requests], replies
    else:
        if replies != [request.encode() + b"\n" for request in requests]:
            view = platform.app(path)
            print("Authorized service diagnostic", json.dumps({
                "power": view["power_state"], "reply_bytes": [len(reply) for reply in replies],
            }), flush=True)
        assert replies == [request.encode() + b"\n" for request in requests], replies
    _, live = platform.context(mid)
    assert live["execution_epoch"] == ref["execution_epoch"] + 1 and live["incarnation_id"] != ref["incarnation_id"]

    def completed_wake():
        events = worker_get(platform, "QueryLifecycleEvents", live, {"after": cursor})["events"]
        restores = [event for event in events if event["origin"] == "wake" and event["kind"] == "vm_restore"]
        wake = {event["operation_id"] for event in restores}
        assert len(wake) <= 1, events
        return next((event["operation_id"] for event in restores if event["stage"] == "completed"), None)

    operation = wait_for(completed_wake, timeout=20)
    restore = platform.native("/operations/" + operation)
    assert restore["statepoint"]["stage"] == "released" and restore["statepoint"]["capture_id"] == capture, restore
    physical = {}
    if case in DIRECT:
        def published():
            current = platform.app(path + "/network")
            return current if current["state"] == "published" else None

        current = wait_for(published, timeout=15)
        assert (current["private_ipv4"], current["attachment_id"], current["mode"]) == (
            address, endpoint["attachment_id"], mode), current
        physical = {"private_ipv4": address, "attachment_id": endpoint["attachment_id"], "source_mode": source_mode}
    lost_response = None
    if case == "http":
        if check_queue:
            next_request = f"owned-{os.urandom(10).hex()}"
            assert invoke(next_request) == (200, None, next_request.encode() + b"\n")
            checks["wake_queue"]["next_request"] = next_request
            requests.append(next_request)
        lost_response = f"close-{os.urandom(10).hex()}"
        status, stage, _ = invoke(lost_response)
        assert (status, stage) == (502, "may_have_started"), (status, stage)
        requests.append(lost_response)
    elif check_forwarding:
        lost_response = f"close-{os.urandom(10).hex()}"
        assert invoke(lost_response) == b""
        next_request = f"owned-{os.urandom(10).hex()}"
        assert invoke(next_request) == next_request.encode() + b"\n"
        checks["tcp_close"] = dict(request=lost_response, response="eof", next_request=next_request)
        requests.extend((lost_response, next_request))
    if check_forwarding:
        assert identity(platform.context(mid)[1]) == identity(live)
        assert completed_wake() == operation
    observed = Counter(platform.guest(live, ["cat", RECEIPTS]).decode().splitlines())
    assert observed == Counter(requests), observed
    witness(platform, case, original=identity(ref), restored=identity(live), capture_id=capture,
            operation_id=operation, business_receipts=dict(observed), mode=mode,
            lost_response=lost_response, **physical, **checks)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "platformd", "system-image"):
        parser.add_argument("--" + name, type=lambda path: Path(path).resolve(), required=True)
    parser.add_argument("--case", choices=sorted(SYSTEM | DIRECT | {"http", "tcp-mtls", "ssh", "cli-terminal"}), required=True)
    parser.add_argument("--mode", choices=("native", "fixed_internal"), default="native")
    parser.add_argument("--sandboxctl", type=lambda path: Path(path).resolve())
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    parser.add_argument("--use-installation", action="store_true", help="read-only bind the supplied installed Bundle")
    parser.add_argument("--check-forwarding", action="store_true",
                        help="check HTTP wake queue admission or TCP EOF after delivery")
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("Linux KVM and an isolated root fixture are required")
    if args.case in {"ssh", "cli-terminal"} and args.sandboxctl is None:
        parser.error("SSH requires the authorized platform client")
    if args.check_forwarding and args.case not in {"http", "tcp-mtls", "private-tcp"}:
        parser.error("--check-forwarding requires http, tcp-mtls, or private-tcp")
    if os.environ.get("OWNED_KVM_SERVICES_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, "-B", __file__, *sys.argv[1:]],
                   {**os.environ, "OWNED_KVM_SERVICES_NAMESPACE": "1", "PYTHONDONTWRITEBYTECODE": "1"})
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="owned-kvm-") as directory, ExitStack() as stack:
        root = Path(directory)
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=8G,mode=0755,nosuid,nodev", "tmpfs", root], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", root], check=True)
        slice_name = f"smolvmowned{os.getpid()}_{os.urandom(4).hex()}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        slice_name, "fail", "1", "MemoryAccounting", "b", "true", "0"],
                       check=True, stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", slice_name], check=True)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = slice_name
        system = args.case in SYSTEM
        keys, tls, gateway = (None, None, None) if system or args.case in DIRECT else tls_setup(root)
        if args.check_forwarding and args.case == "http":
            gateway["max_waiters_per_machine"] = HTTP_WAKE_WAITERS
        platform = Platform(args, root, stack, () if system else (NETWORK,),
                            use_installation=args.use_installation, tls=tls, gateway=gateway,
                            managed_ssh=args.case in {"ssh", "cli-terminal"})
        try:
            platform.start()
            if system:
                system_lifecycle(platform, args.case)
            elif args.case == "cli-terminal":
                from polish_cli_terminal import check
                check(platform, args, keys, prepare_openssh(platform, keys))
            else:
                ssh_key = prepare_openssh(platform, keys) if args.case == "ssh" else None
                service_access(platform, args.case, keys, ssh_key, args.sandboxctl, args.mode)
        except BaseException:
            for name in ("platform.log", "worker.log", "serve.log"):
                log = root / name
                if log.exists():
                    print(name, log.read_text(errors="replace")[-1500:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
