#!/usr/bin/env python3
"""Exercise ordinary authorized service access and owned lifecycle operations on KVM."""

import argparse
from collections import Counter
from concurrent.futures import ThreadPoolExecutor
from contextlib import ExitStack
import json
import os
from pathlib import Path
import shlex
import socket
import ssl
import subprocess
import sys
import tempfile
from threading import Barrier

from managed_system_vsock import Platform, wait_for, worker


DIRECT = {"private-tcp", "private-udp"}
SYSTEM = {"system-boot", "system-fork", "system-restore"}
NETWORK = dict(network_id="owned-services", route_domain_id="owned-services", cidr="10.44.61.0/24")
PROGRAM = "/run/authorized-service.pl"
RECEIPTS = "/run/authorized-service-receipts"
SERVICE_PORT = 18081


def identity(ref):
    return {key: ref[key] for key in ("instance_id", "incarnation_id", "execution_epoch")}


def witness(platform, case, **facts):
    print("Authorized KVM", json.dumps(dict(case=case, bundle_digest=platform.bundle_id, **facts), sort_keys=True), flush=True)


def machine(platform, label, mode=None):
    resources = dict(cpu_count=1, memory_mib=512) if mode else dict(cpu_count=2, memory_mib=768)
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


def stop(platform, mid):
    path = f"machines/{mid}"
    def running():
        view = platform.app(path)
        return view if view["power_state"] == "running" else None

    platform.complete(platform.change(path + "/stop", "stop-" + mid, {}, wait_for(running, timeout=15)))


def system_lifecycle(platform, case):
    mid, _, ref = machine(platform, "system")
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

        def http(request):
            with ExitStack() as clients:
                connection = platform.api_connection(timeout=65)
                clients.callback(connection.close)
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
    requests = [f"owned-{os.urandom(10).hex()}" for _ in range(1 if protocol == "udp" else 2)]
    barrier = Barrier(len(requests), timeout=12)

    def first_request(request):
        barrier.wait()
        return invoke(request)

    with ThreadPoolExecutor(max_workers=len(requests)) as clients:
        replies = list(clients.map(first_request, requests))
    if case == "http":
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
        wake = {event["operation_id"] for event in events if event["origin"] == "wake"}
        assert len(wake) <= 1, events
        if wake and any(event["origin"] == "wake" and event["stage"] == "completed" for event in events):
            return wake.pop()

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
        lost_response = f"close-{os.urandom(10).hex()}"
        status, stage, _ = invoke(lost_response)
        assert (status, stage) == (502, "may_have_started"), (status, stage)
        requests.append(lost_response)
    observed = Counter(platform.guest(live, ["cat", RECEIPTS]).decode().splitlines())
    assert observed == Counter(requests), observed
    witness(platform, case, original=identity(ref), restored=identity(live), capture_id=capture,
            operation_id=operation, business_receipts=dict(observed), mode=mode,
            lost_response=lost_response, **physical)
    stop(platform, mid)
    if case in DIRECT:
        stop(platform, source)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "platformd", "system-image"):
        parser.add_argument("--" + name, type=lambda path: Path(path).resolve(), required=True)
    parser.add_argument("--case", choices=sorted(SYSTEM | DIRECT | {"http", "tcp-mtls", "ssh"}), required=True)
    parser.add_argument("--mode", choices=("native", "fixed_internal"), default="native")
    parser.add_argument("--sandboxctl", type=lambda path: Path(path).resolve())
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("Linux KVM and an isolated root fixture are required")
    if args.case == "ssh" and args.sandboxctl is None:
        parser.error("SSH requires the authorized platform client")
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
        platform = Platform(args, root, stack, () if system else (NETWORK,),
                            tls=tls, gateway=gateway, managed_ssh=args.case == "ssh")
        try:
            platform.start()
            if system:
                system_lifecycle(platform, args.case)
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
