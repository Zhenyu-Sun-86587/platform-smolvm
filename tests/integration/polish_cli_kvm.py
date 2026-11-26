#!/usr/bin/env python3
"""Qualify the J11 CLI workflows on one isolated, installed System Worker."""

import argparse
from contextlib import closing, ExitStack
import hashlib
from http.client import HTTPSConnection
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile

from authorized_services_kvm import NETWORK, PROGRAM, RECEIPTS, SERVICE_PORT, tls_setup
from managed_system_vsock import Platform, digest, wait_for


RAM_PATH = "/run/j11-state"
DISK_PATH = "/root/j11-state"
FILE_PATH = "/root/j11-file.bin"
NAMESPACE_ORIGIN = "POLISH_CLI_PARENT_NAMESPACES"


class CLI:
    def __init__(self, platform, ca):
        credential = platform.change(
            "tokens", "j11-cli-owner", {"label": "J11 CLI owner", "scope": {"access": "manage"}}, status=200,
        )
        self.platform = platform
        self.env = {
            **os.environ,
            "SANDBOX_URL": platform.origin,
            "SANDBOX_CA_FILE": str(ca),
            "SANDBOX_TOKEN": credential["token"],
        }

    def run(self, *args, expected=0):
        result = subprocess.run(
            [self.platform.args.sandboxctl, *args], env=self.env,
            stdin=subprocess.DEVNULL, capture_output=True, check=False, timeout=330,
        )
        if result.returncode != expected:
            raise AssertionError({
                "command": args, "expected_exit": expected, "exit": result.returncode,
                "stdout": result.stdout[:3000], "stderr": result.stderr[:3000],
            })
        return result

    def json(self, *args):
        return json.loads(self.run("--json", *args).stdout)

    def operation(self, *args, observe_timeout=False):
        accepted = self.json("--no-wait", *args)
        operation_id = accepted["operation_id"]
        assert accepted["state"] == "queued" and accepted["effect"] == "none", accepted
        if observe_timeout:
            response = self.run(
                "--json", "--wait-timeout", "1ns", "operations", "wait", operation_id, expected=124,
            )
            error = json.loads(response.stderr)["error"]
            assert (error["code"], error["operation_id"], error["retry"]) == (
                "OPERATION_WAIT_TIMEOUT", operation_id, "poll_operation",
            ), error
        # Waiting uses the accepted ID even when local observation timed out.
        finished = self.json("--wait-timeout", "5m", "operations", "wait", operation_id)
        assert finished["operation_id"] == operation_id, finished
        assert finished["state"] == "succeeded" and finished["effect"] == "applied", finished
        readback = self.json("operations", "get", operation_id)
        for key in ("state", "stage", "effect", "result"):
            assert readback[key] == finished[key], (key, finished, readback)
        print(json.dumps({
            "steps": args[:2], "operation_id": operation_id, "kind": finished["kind"],
            "accepted_state": accepted["state"], "completed_state": finished["state"],
            "effect": finished["effect"], "resumed_after_local_timeout": observe_timeout,
        }), flush=True)
        return finished

    def guest(self, machine, *command):
        reply = self.run("exec", "--follow", "--cwd", "/", "--timeout-seconds", "30", machine, "--", *command)
        return reply.stdout

    def ready_machine(self, machine):
        def ready():
            view = self.json("machines", "show", machine)
            if view["power_state"] == "running" and view["guest_management_status"] == "ready":
                return view

        return wait_for(ready, timeout=60)

    def execution(self, machine):
        view = self.ready_machine(machine)
        binding = view["binding"]
        ref = self.platform.native(f"/machines/{binding['runtime_name']}/execution")
        assert (binding["runtime_instance_object_id"], binding["incarnation_id"], binding["execution_epoch"]) == (
            ref["instance_id"], ref["incarnation_id"], ref["execution_epoch"],
        ), (binding, ref)
        return ref

    def idle_access(self, machine):
        wait_for(lambda: self.json("machines", "metrics", machine)["platform"]["active_access_holds"] == 0, timeout=30)

    def delete(self, machine):
        self.idle_access(machine)
        stopped = self.operation("machines", "stop", machine)
        assert stopped["source_disposition"] == "EXITED", stopped
        assert self.json("machines", "show", machine)["power_state"] == "stopped"
        deleted = self.operation("machines", "delete", machine, "--yes")
        remaining = self.json("machines", "list")["items"]
        assert machine not in {item["machine_id"] for item in remaining}, remaining
        return deleted["operation_id"]


def check_exec_and_files(cli, machine):
    streamed = cli.run(
        "exec", "--follow", "--wait-seconds", "0", "--cwd", "/", machine, "--",
        "/bin/sh", "-c", r"printf '\000\377out'; sleep 1; printf '\376\000err' >&2; exit 17",
        expected=17,
    )
    assert streamed.stdout == b"\0\xffout", streamed.stdout
    assert streamed.stderr == b"\xfe\0err", streamed.stderr
    contents = bytes(range(256)) * 2800 + b"\0\xff\r\n"
    local = cli.platform.root / "j11-upload"
    local.write_bytes(contents)
    expected = hashlib.sha256(contents).hexdigest()
    capabilities = wait_for(
        lambda: caps if (caps := cli.json("files", "capabilities", machine)).get(
            "files.write", {}).get("supported") else None,
    )
    assert capabilities["files.write"]["constraints"]["transport"] == "native_agent", capabilities
    result = cli.json("files", "put", machine, str(local), FILE_PATH)
    assert (result["state"], result["path"], result["size"], result["sha256"]) == (
        "uploaded", FILE_PATH, len(contents), expected,
    ), result
    listing = cli.json("files", "list", machine, "/root")["items"]
    item, = [item for item in listing if item["name"] == Path(FILE_PATH).name]
    assert item["type"] == "file" and item["size"] == len(contents), item
    assert cli.guest(machine, "sha256sum", FILE_PATH).split()[0].decode() == expected
    downloaded = cli.run("files", "get", machine, FILE_PATH, "-").stdout
    assert downloaded == contents and len(downloaded) > 512 << 10
    return {"size": len(contents), "sha256": expected, "upload_id": result["upload_id"], "exec_exit": 17}


def markers(cli, machine, ram, disk):
    assert cli.guest(machine, "/bin/cat", RAM_PATH, DISK_PATH) == (ram + disk).encode()


def write_markers(cli, machine, ram, disk):
    assert cli.guest(
        machine, "/bin/sh", "-ec",
        'printf %s "$1" >"$3"; printf %s "$2" >"$4"; sync "$4"',
        "j11-marker", ram, disk, RAM_PATH, DISK_PATH,
    ) == b""


def check_fork(cli, parent, source):
    assert cli.guest(parent, "/usr/bin/stat", "-f", "-c", "%T", "/run").strip() == b"tmpfs"
    write_markers(cli, parent, "fork-ram:", "fork-parent-disk")
    cli.idle_access(parent)
    fork = cli.operation("machines", "live-fork", parent, "J11 Fork child")
    child, = fork["children"]
    assert fork["source_lock_released"] and fork["source_disposition"] == "RUNNING", fork
    assert child["state"] == "ready" and child["resource_independence"] == "verified", child
    mid = child["machine_id"]
    execution = cli.execution(mid)
    assert execution["incarnation_id"] != source["incarnation_id"], execution
    assert cli.execution(parent) == source
    markers(cli, mid, "fork-ram:", "fork-parent-disk")
    write_markers(cli, mid, "fork-child-ram:", "fork-child-disk")
    markers(cli, mid, "fork-child-ram:", "fork-child-disk")
    markers(cli, parent, "fork-ram:", "fork-parent-disk")
    deleted = cli.delete(mid)
    return {
        "operation_id": fork["operation_id"], "child": mid, "child_execution": execution,
        "capture_id": fork["result"]["capture_id"], "delete_operation_id": deleted,
    }


def check_snapshots(cli, machine, source):
    write_markers(cli, machine, "snapshot-ram:", "snapshot-disk")
    cli.idle_access(machine)
    saved = cli.operation("snapshots", "capture-vm", machine, "--label", "J11 VM state")
    snapshot = saved["result"]["snapshot_id"]
    description = cli.json("snapshots", "get", snapshot)
    provenance = description["vm_provenance"]
    summary = description["capture_summary"]
    assert description["capture_mode"] == "vm" and description["state"] == "ready", description
    assert provenance["operation_id"] == saved["operation_id"], provenance
    assert provenance["runtime_bundle_id"] == cli.platform.bundle_id and provenance["state_reader_id"], provenance
    assert summary["ram"] == "included" and summary["configuration"]["status"] == "verified", summary
    assert any(disk["role"] == "root" for disk in summary["disks"]), summary
    assert cli.execution(machine) == source

    write_markers(cli, machine, "after-snapshot-ram:", "after-snapshot-disk")
    cli.idle_access(machine)
    rolled = cli.operation("snapshots", "rollback-vm", machine, snapshot)
    result = rolled["result"]
    assert rolled["source_disposition"] == "EXITED" and result["rescue_capture_id"], rolled
    assert result["statepoint"]["capture_id"] == provenance["runtime_capture_id"], result
    previous = cli.execution(machine)
    assert previous["incarnation_id"] != source["incarnation_id"]
    markers(cli, machine, "snapshot-ram:", "snapshot-disk")

    restored = cli.operation("snapshots", "restore-vm", snapshot, "J11 restored workspace")
    result = restored["result"]
    restored_id = result["machine_id"]
    restored_execution = cli.execution(restored_id)
    assert restored_id != machine and restored_execution["incarnation_id"] != previous["incarnation_id"]
    assert result["statepoint"]["capture_id"] == provenance["runtime_capture_id"], result
    markers(cli, restored_id, "snapshot-ram:", "snapshot-disk")
    write_markers(cli, restored_id, "restore-only-ram:", "restore-only-disk")
    markers(cli, machine, "snapshot-ram:", "snapshot-disk")
    restored_deleted = cli.delete(restored_id)
    return previous, {
        "snapshot_id": snapshot, "capture_id": provenance["runtime_capture_id"],
        "capture_operation_id": saved["operation_id"], "rollback_operation_id": rolled["operation_id"],
        "restore_operation_id": restored["operation_id"], "restored_execution": restored_execution,
        "restored_delete_operation_id": restored_deleted,
    }


class LocalHTTPS(HTTPSConnection):
    def connect(self):
        transport = socket.create_connection(("127.0.0.1", self.port), timeout=self.timeout)
        try:
            self.sock = self._context.wrap_socket(transport, server_hostname=self.host)
        except BaseException:
            transport.close()
            raise


def check_service(cli, machine):
    platform = cli.platform
    service = cli.operation("services", "create", machine, "J11 HTTPS", "--protocol", "http",
                            "--guest-port", str(SERVICE_PORT))["result"]["service_id"]
    configured = cli.json("services", "status", service)
    assert configured["observation"]["status"] == "unknown", configured
    host = configured["public_host"]
    assert host.endswith("." + platform.gateway["apps_domain"]), configured
    token = platform.change(
        "tokens", "j11-tunnel", {"label": "J11 HTTPS", "scope": {"access": "tunnel", "machine_id": machine}},
        status=200,
    )["token"]

    def receipt(request_id, bearer=None):
        headers = {"Authorization": "Bearer " + bearer} if bearer else {}
        with closing(LocalHTTPS(host, platform.port, context=platform.tls[2], timeout=65)) as client:
            client.request("POST", "/receipt/" + request_id, b"", headers)
            with client.getresponse() as reply:
                return reply.status, reply.headers, reply.read()

    denied, _, body = receipt("denied-j11")
    assert denied == 401 and json.loads(body)["error"]["effect"] == "none", (denied, body)
    status, headers, body = receipt("not-listening-j11", token)
    assert (status, headers.get("X-Sandbox-Forwarding"), headers.get("X-Sandbox-Upstream-Reason")) == (
        502, "not_started", "UPSTREAM_CONNECTION_REFUSED",
    ), (status, headers, body)
    observed = cli.json("services", "status", service)["observation"]
    assert (observed["status"], observed["scope"], observed["reason_code"]) == (
        "unavailable", "worker_upstream", "UPSTREAM_CONNECTION_REFUSED",
    ), observed

    fixture = Path(__file__).resolve().parents[1] / "fixtures/authorized-service.pl"
    cli.json("files", "put", machine, str(fixture), PROGRAM)
    cli.guest(machine, "/usr/bin/systemd-run", "--quiet", "--unit=j11-service", "/usr/bin/perl",
              PROGRAM, "http", str(SERVICE_PORT))

    def listening():
        rows = cli.guest(machine, "/bin/cat", "/proc/net/tcp").decode().splitlines()[1:]
        return any(row.split()[1].endswith(f":{SERVICE_PORT:04X}") and row.split()[3] == "0A" for row in rows)

    wait_for(listening, timeout=20)
    request_id = "j11-" + os.urandom(8).hex()
    status, _, data = receipt(request_id, token)
    assert status == 200 and data == (request_id + "\n").encode(), (status, data)
    assert cli.guest(machine, "/bin/cat", RECEIPTS) == data, "only the successful POST may have a Guest receipt"
    ready = cli.json("services", "status", service)["observation"]
    assert ready["status"] == "ready" and ready["scope"] == "worker_upstream", ready
    operation = cli.operation("services", "delete", service)
    cli.guest(machine, "/usr/bin/systemctl", "stop", "j11-service")
    return {
        "service_id": service, "receipt_id": request_id, "receipt_count": 1, "tls_hostname": host,
        "unauthorized_status": denied, "unlistened_reason": observed["reason_code"],
        "delete_operation_id": operation["operation_id"],
    }


def check_hibernate(cli, machine, source):
    write_markers(cli, machine, "hibernate-ram:", "hibernate-disk")
    cli.idle_access(machine)
    sleeping = cli.operation("machines", "hibernate", machine)
    assert sleeping["source_disposition"] == "EXITED", sleeping
    assert cli.json("machines", "show", machine)["power_state"] == "hibernated"
    capture = sleeping["result"]["capture_id"]
    wake = cli.operation("machines", "wake-full", machine, "--from", "hibernated")
    assert wake["result"]["statepoint"]["capture_id"] == capture, wake
    successor = cli.execution(machine)
    assert successor["instance_id"] == source["instance_id"]
    assert successor["incarnation_id"] != source["incarnation_id"]
    assert successor["execution_epoch"] == source["execution_epoch"] + 1
    markers(cli, machine, "hibernate-ram:", "hibernate-disk")
    return {
        "capture_id": capture, "hibernate_operation_id": sleeping["operation_id"],
        "wake_operation_id": wake["operation_id"], "successor": successor,
    }


def check_workflow(platform, ca):
    cli = CLI(platform, ca)
    created = cli.operation(
        "machines", "create", "J11 native workspace", "--template", "t_system", "--boot-mode", "system",
        "--runtime-instance", platform.serve_id, "--network", NETWORK["network_id"], "--network-mode", "native",
        "--cpu", "1", "--memory-mib", "512", observe_timeout=True,
    )
    machine = created["machine_id"]
    source = cli.execution(machine)
    assert source["execution_epoch"] == 1, source
    resources = cli.json("machines", "resources", machine)["requested"]
    assert resources["cpu_count"] == 1 and resources["memory_mib"] == 512, resources
    cli.operation("lifecycle", "set", machine, "--policy", '{"auto_standby":false,"auto_hibernate":false}')
    facts = {"exec_and_files": check_exec_and_files(cli, machine), "live_fork": check_fork(cli, machine, source)}
    previous, facts["snapshots"] = check_snapshots(cli, machine, source)
    facts["service"] = check_service(cli, machine)
    facts["hibernate"] = check_hibernate(cli, machine, previous)
    facts["delete_operation_id"] = cli.delete(machine)
    assert cli.json("machines", "list")["items"] == []
    print(json.dumps({
        "case": "J11-cli", "bundle": platform.bundle_id, "sandboxctl_sha256": digest(platform.args.sandboxctl),
        "platformd_sha256": digest(platform.args.platformd), "source": source, "machine_id": machine,
        "create_operation_id": created["operation_id"], **facts,
    }, sort_keys=True), flush=True)


def isolated_namespaces(parser):
    current = ":".join(str(Path("/proc/self/ns", name).stat().st_ino) for name in ("mnt", "net"))
    origin = os.environ.get(NAMESPACE_ORIGIN)
    if origin is None:
        os.execvpe("unshare", [
            "unshare", "--mount", "--propagation", "private", "--net", "--",
            sys.executable, "-B", str(Path(__file__).resolve()), *sys.argv[1:],
        ], {**os.environ, NAMESPACE_ORIGIN: current})
    if len(origin.split(":")) != 2 or any(a == b for a, b in zip(origin.split(":"), current.split(":"))):
        parser.error("fixture requires its own mount and network namespaces")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ("platformd", "sandboxctl", "bundle", "system-image"):
        parser.add_argument("--" + flag, type=lambda p: Path(p).resolve(strict=True), required=True)
    parser.add_argument("--work-dir", type=lambda p: Path(p).resolve(strict=True), default=Path("/tmp"))
    args = parser.parse_args()
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("requires Linux root and KVM")
    if not Path("/run/smolvm-custom").is_dir():
        parser.error("requires the existing /run/smolvm-custom fixture mount target")
    isolated_namespaces(parser)
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="j11-") as scratch, ExitStack() as stack:
        root = Path(scratch)
        root.chmod(0o755)
        unit = f"smolvmcli{os.getpid()}_{os.urandom(4).hex()}.slice"
        subprocess.run([
            "busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
            "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
            unit, "fail", "1", "MemoryAccounting", "b", "true", "0",
        ], check=True, stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", unit], check=True)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = unit
        keys, tls, gateway = tls_setup(root)
        platform = Platform(args, root, stack, (NETWORK,), state_tmpfs=True, use_installation=True,
                            tls=tls, gateway=gateway)
        try:
            platform.start()
            check_workflow(platform, keys / "ca.pem")
        except BaseException:
            for log in (root / "platform.log", root / "worker.log", root / "serve.log"):
                if log.exists():
                    print(f"{log.name}\n{log.read_text(errors='replace')[-3500:]}", file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
