#!/usr/bin/env python3
"""Verify owned service streams across revision, revocation and SSH resume."""

import argparse
from collections import Counter
from contextlib import ExitStack
from http.client import IncompleteRead
import os
from pathlib import Path
import secrets
import select
import shlex
import socket
import ssl
import subprocess
import sys
import tempfile
import time

from authorized_services_kvm import NETWORK, Platform, create_service, identity, machine, prepare_openssh, tls_setup, token, witness
from managed_system_vsock import digest, wait_for


PORTS = (18111, 18112)
PROGRAM = "/run/service-revision.pl"
RECEIPTS = "/run/service-revision-receipts"
AFTER_REVOCATION = "/run/service-revision-after-revoke"
BACKEND = r'''
use strict;
use warnings;
use IO::Socket::INET;
use Fcntl qw(O_WRONLY O_CREAT O_APPEND);

my ($protocol, $port, $backend) = @ARGV;
my $listener = IO::Socket::INET->new(LocalAddr => '0.0.0.0', LocalPort => $port,
                                    Proto => 'tcp', ReuseAddr => 1, Listen => 16) or die $!;
$SIG{CHLD} = 'IGNORE';
while (my $client = $listener->accept()) {
    my $pid = fork();
    die $! unless defined $pid;
    if ($pid) {
        close $client;
        next;
    }
    close $listener;
    $client->autoflush(1);
    my $first = <$client> // '';
    my $id;
    if ($protocol eq 'http') {
        $id = $1 if $first =~ /^GET \/timeline\/([a-z0-9-]{1,64}) HTTP\/1\.[01]\r?\n$/;
        while (my $header = <$client>) { last if $header =~ /^\r?\n$/; }
    } else {
        $id = $1 if $first =~ /^([a-z0-9-]{1,64})\n$/;
    }
    exit unless defined $id;
    sysopen(my $receipts, '/run/service-revision-receipts', O_WRONLY | O_CREAT | O_APPEND, 0600) or die $!;
    my $receipt = "$backend $id\n";
    syswrite($receipts, $receipt) == length($receipt) or die $!;
    close $receipts;
    if ($protocol eq 'http') {
        print {$client} "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n";
    }
    my $count = 0;
    while (1) {
        my $state = -e '/run/service-revision-after-revoke' ? 'after-revoke' : 'active';
        my $frame = join(':', $backend, $id, ++$count, $state);
        my $line = $protocol eq 'http' ? "data: $frame\n\n" : "$frame\n";
        last unless print {$client} $line;
        select(undef, undef, undef, 0.15);
    }
    exit;
}
'''


class Timeline:
    def __init__(self, stream, backend, request_id):
        self.stream = stream
        self.backend = backend
        self.request_id = request_id
        self.position = 0
        self.state = ""

    def read(self):
        while (line := self.stream.readline()) in (b"\n", b"\r\n"):
            pass
        if not line:
            return False
        backend, request_id, number, state = line.removeprefix(b"data: ").decode().strip().split(":")
        assert (backend, request_id) == (self.backend, self.request_id), line
        assert int(number) == self.position + 1, (self.position, line)
        self.position = int(number)
        self.state = state
        return True

    def advance(self):
        assert self.read() and self.read(), (self.backend, self.request_id, self.position)

    def close_after(self, changed_at):
        cutoff = changed_at + 5
        while time.monotonic() < cutoff:
            try:
                if self.read():
                    continue
            except (ConnectionResetError, IncompleteRead, ssl.SSLEOFError):
                pass
            return round((time.monotonic() - changed_at) * 1000)
        raise AssertionError(("superseded stream remained open", self.backend, self.request_id, self.position))


def credentials(platform, mid, service, protocol, keys, label):
    if protocol == "http":
        issued = platform.change("tokens", "a22-token-" + label, {
            "label": label, "scope": {"access": "tunnel", "machine_id": mid},
        }, status=200)
        return "tokens/" + issued["token_id"], issued["revision"], issued["token"]
    issued = platform.change("client-certificates", "a22-certificate-" + label, {
        "csr": (keys / "client.csr").read_text(), "scope": {"service_ids": [service["service_id"]]}, "ttl_seconds": 600,
    }, status=200)
    certificate = keys / (label + ".pem")
    certificate.write_text(issued["certificate_pem"])
    tls = ssl.create_default_context(cafile=str(keys / "ca.pem"))
    tls.load_cert_chain(certificate, keys / "client.key")
    return "client-certificates/" + issued["certificate_id"], issued["revision"], tls


def connect(platform, clients, service, credential, protocol, backend):
    request_id = "revision-" + secrets.token_hex(10)
    if protocol == "http":
        connection = platform.api_connection(timeout=5)
        clients.callback(connection.close)
        connection.request("GET", "/timeline/" + request_id, headers={
            "Host": service["public_host"], "Authorization": "Bearer " + credential,
        })
        reply = connection.getresponse()
        assert reply.status == 200, (reply.status, reply.headers)
        stream = clients.enter_context(reply)
    else:
        raw = clients.enter_context(socket.create_connection(("127.0.0.1", service["public_port"]), timeout=5))
        connection = clients.enter_context(credential.wrap_socket(raw, server_hostname="127.0.0.1"))
        connection.sendall(request_id.encode() + b"\n")
        stream = clients.enter_context(connection.makefile("rb"))
    live = Timeline(stream, backend, request_id)
    live.advance()
    return live


def mutate(platform, method, path, key, revision, data, status):
    return platform.app(path, method, data, {"If-Control-Epoch": platform.epoch, "Idempotency-Key": key,
                                           "If-Match": f'"{revision}"'}, status=status)


def revise(platform, service, edit):
    path = "services/" + service["service_id"]
    previous = service["revision"]
    result = platform.complete(mutate(platform, "PATCH", path, "a22-revision", previous, edit, 202))
    current = platform.app(path)
    assert result["effect"] == "applied" and current["revision"] == previous + 1, (result, current)
    return current


def revision_case(platform, keys, protocol, observe=False):
    mid, _, ref = machine(platform, "owned revision service", "native")
    platform.guest(ref, ["/bin/sh", "-ec", 'cat > "$1"', "owned", PROGRAM], stdin=BACKEND)
    sid = create_service(platform, mid, protocol, PORTS[0])
    service_path = "services/" + sid
    before = platform.app(service_path)
    if protocol == "tcp-mtls":
        wait_for(lambda: f":{before['public_port']} " in subprocess.check_output(["ss", "-ltn"], text=True))
    revoked_path, revocation_revision, original = credentials(platform, mid, before, protocol, keys, "selected")
    _, _, survivor = credentials(platform, mid, before, protocol, keys, "separate")

    if observe:
        assert before["observation"]["status"] == "unknown", before
        machine_path = "machines/" + mid
        platform.complete(platform.change(machine_path + "/standby", "park-before-observation", {},
                                          platform.wait_capability(machine_path, "lifecycle.standby")))
        for _ in range(3):
            assert platform.app(service_path)["observation"] == before["observation"]
            platform.app(machine_path + "/metrics")
            inventory = platform.native(f"/instances/{ref['instance_id']}/inventory")["inventory"]
            assert inventory["source_disposition"] == "STANDBY", inventory
        # A real request wakes the source and tries a port with no listener.
        client = platform.api_connection(timeout=30)
        try:
            client.request("GET", "/timeline/before-start", headers={
                "Host": before["public_host"], "Authorization": "Bearer " + original})
            reply = client.getresponse()
            assert reply.status >= 400, (reply.status, reply.read())
            reply.read()
        finally:
            client.close()
        negative = platform.app(service_path)["observation"]
        assert negative["stage"] == "worker_upstream" and negative["status"] == "unavailable", negative
        assert negative["execution_epoch"] == ref["execution_epoch"] and negative["observed_at"], negative
        print("Unlistened upstream", negative, flush=True)

    for label, port in zip(("before", "after"), PORTS, strict=True):
        platform.guest(ref, ["systemd-run", "--unit=a22-" + label, "/usr/bin/perl", PROGRAM, protocol, str(port), label])
        wait_for(lambda: f":{port:04X}".encode() in platform.guest(ref, ["cat", "/proc/net/tcp"]))
    with ExitStack() as clients:
        old = connect(platform, clients, before, original, protocol, "before")
        if observe:
            live = platform.app(service_path)["observation"]
            assert live["scope"] == "worker_upstream" and live["status"] == "ready", live
            assert live["service_revision"] == before["revision"] and live["execution_epoch"] == ref["execution_epoch"]
        changed = time.monotonic()
        after = revise(platform, before, {"guest_port": PORTS[1]})
        assert after["guest_port"] == PORTS[1], after
        if observe:
            assert after["observation"]["status"] == "stale" and after["observation"]["service_revision"] == before["revision"], after
        revision_close_ms = old.close_after(changed)
        selected = connect(platform, clients, after, original, protocol, "after")
        unaffected = connect(platform, clients, after, survivor, protocol, "after")
        selected.advance()
        revoked = time.monotonic()
        receipt = mutate(platform, "DELETE", revoked_path, "a22-revoke", revocation_revision, {}, 200)
        assert receipt["status"] == "revoked" and receipt["revision"] == revocation_revision + 1, receipt
        revoke_close_ms = selected.close_after(revoked)
        uninterrupted = unaffected.position
        platform.guest(ref, ["/usr/bin/touch", AFTER_REVOCATION])
        wait_for(lambda: unaffected.read() and unaffected.state == "after-revoke", timeout=5)
        assert unaffected.position > uninterrupted, unaffected.position
        expected = Counter(f"{stream.backend} {stream.request_id}" for stream in (old, selected, unaffected))
        observed = Counter(platform.guest(ref, ["cat", RECEIPTS]).decode().splitlines())
        assert observed == expected, (expected, observed)
        assert identity(platform.context(mid)[1]) == identity(ref)
        witness(platform, protocol + "-revision", execution=identity(ref), service_id=sid,
                revisions=[before["revision"], after["revision"]], guest_ports=list(PORTS),
                revision_close_ms=revision_close_ms, credential_close_ms=revoke_close_ms,
                surviving_frames=[uninterrupted, unaffected.position], business_receipts=dict(observed))


def ssh_case(platform, keys):
    ssh_key = prepare_openssh(platform, keys)
    mid, _, original = machine(platform, "owned SSH revision", "native")
    machine_path = f"machines/{mid}"
    sid = create_service(platform, mid, "ssh", 2222)
    service_path = f"services/{sid}"
    before = platform.app(service_path)
    host_key = wait_for(lambda: platform.app(service_path).get("ssh_host_key"), retry=(OSError, RuntimeError))
    hostname = "owned-" + mid
    known = keys / "known_hosts"
    known.write_text(f"{hostname} {host_key}\n")
    environment = {**os.environ, "SANDBOX_TOKEN": token(platform, mid), "SANDBOX_URL": platform.origin,
                   "SANDBOX_CA_FILE": str(keys / "ca.pem")}
    proxy = f"{shlex.quote(str(platform.args.sandboxctl))} ssh-proxy --machine {mid}"
    ssh = ["ssh", "-F", "/dev/null", "-i", ssh_key, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
           "-o", "StrictHostKeyChecking=yes", "-o", f"UserKnownHostsFile={known}", "-o", f"HostKeyAlias={hostname}",
           "-o", f"ProxyCommand={proxy}", "root@workspace"]
    marker = "ssh-revision-" + secrets.token_hex(12)
    stream = "i=0; while :; do i=$((i+1)); printf 'a22-ssh:%s\\n' \"$i\"; sleep .15; done"
    with subprocess.Popen([*ssh, stream], env=environment, stdout=subprocess.PIPE,
                          stderr=subprocess.PIPE, bufsize=0) as client:
        try:
            for position in (1, 2):
                assert select.select([client.stdout], [], [], 20)[0], "SSH stream never started"
                assert client.stdout.readline() == f"a22-ssh:{position}\n".encode()
            started = time.monotonic()
            after = revise(platform, before, {"name": "owned SSH after revision"})
            assert client.wait(timeout=5) != 0, "SSH stream ended successfully before the fixture closed it"
            revision_close_ms = round((time.monotonic() - started) * 1000)
        finally:
            if client.poll() is None:
                client.terminate()
                client.wait(timeout=10)

    store = f"printf '%s' {shlex.quote(marker)} >/run/a22-ssh-state; cat /run/a22-ssh-state"
    assert subprocess.check_output([*ssh, store], env=environment, timeout=35) == marker.encode()
    assert identity(platform.context(mid)[1]) == identity(original)
    inventory = platform.native(f"/instances/{original['instance_id']}/inventory")
    with os.fdopen(os.pidfd_open(inventory["execution"]["pid"]), "rb", buffering=0) as pid:
        sleeping = platform.complete(platform.change(machine_path + "/hibernate", "a22-ssh-hibernate", {},
                                                      platform.app(machine_path)))
        assert select.select([pid], [], [], 5)[0], "the retired VMM remained alive"
        assert platform.app(machine_path)["power_state"] == "hibernated"
    capture = sleeping["result"]["statepoint"]["capture_id"]
    assert subprocess.check_output([*ssh, "cat /run/a22-ssh-state"], env=environment, timeout=65) == marker.encode()
    current = platform.context(mid)[1]
    assert current["instance_id"] == original["instance_id"] and current["incarnation_id"] != original["incarnation_id"]
    assert current["execution_epoch"] == original["execution_epoch"] + 1
    assert platform.app(service_path)["ssh_host_key"] == host_key

    def wake_completed():
        events = worker_get(platform, "QueryLifecycleEvents", current, {"after": 0})["events"]
        restores = [event for event in events if event["origin"] == "wake" and event["kind"] == "vm_restore"]
        assert len({event["operation_id"] for event in restores}) <= 1, events
        return next((event["operation_id"] for event in restores if event["stage"] == "completed"), None)

    wake = wait_for(wake_completed, timeout=20)
    receipt = platform.native("/operations/" + wake)["statepoint"]
    assert receipt["stage"] == "released" and receipt["capture_id"] == capture, receipt
    witness(platform, "ssh-revision", original=identity(original), restored=identity(current), service_id=sid,
            revisions=[before["revision"], after["revision"]], revision_close_ms=revision_close_ms,
            retired_vmm=inventory["execution"]["pid"], capture_id=capture, wake_operation_id=wake, guest_marker=marker)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "platformd", "system-image"):
        parser.add_argument("--" + name, type=lambda value: Path(value).resolve(strict=True), required=True)
    parser.add_argument("--case", choices=("http", "tcp-mtls", "ssh"), required=True)
    parser.add_argument("--sandboxctl", type=lambda value: Path(value).resolve(strict=True))
    parser.add_argument("--check-observations", action="store_true", help="check passive reads and actual HTTP upstream evidence")
    args = parser.parse_args()
    if args.check_observations and args.case != "http":
        parser.error("service observation checks use HTTP")
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("requires root and KVM")
    if args.case == "ssh" and args.sandboxctl is None:
        parser.error("SSH requires the authorized sandboxctl client")
    if os.environ.get("OWNED_KVM_REVISION_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, "-B", __file__, *sys.argv[1:]],
                   {**os.environ, "OWNED_KVM_REVISION_NAMESPACE": "1", "PYTHONDONTWRITEBYTECODE": "1"})
    with tempfile.TemporaryDirectory(dir="/var/tmp", prefix="a22-revision-") as directory, ExitStack() as stack:
        root = Path(directory)
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=4G,mode=0755,nosuid,nodev", "tmpfs", root], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", root], check=True)
        unit = f"smolvmrevision{os.getpid()}_{secrets.token_hex(4)}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        unit, "fail", "1", "MemoryAccounting", "b", "true", "0"],
                       check=True, stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", unit], check=True)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = unit
        keys, tls, gateway = tls_setup(root)
        platform = Platform(args, root, stack, (NETWORK,), state_tmpfs=True, use_installation=True,
                            tls=tls, gateway=gateway, managed_ssh=args.case == "ssh")
        try:
            platform.start()
            print(f"Platform {digest(args.platformd)}; System image {digest(args.system_image)}.", flush=True)
            if args.case == "ssh":
                ssh_case(platform, keys)
            else:
                revision_case(platform, keys, args.case, args.check_observations)
        except BaseException:
            for log in root.glob("*.log"):
                print(log.name, log.read_text(errors="replace")[-2000:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
