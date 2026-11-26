#!/usr/bin/env python3
"""Verify an owned System target's activation interruption with real KVM I/O."""

import argparse
import base64
from contextlib import ExitStack, closing
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import select
import signal
import sqlite3
import subprocess
import sys
import tarfile
import tempfile
import time

from authorized_services_kvm import NETWORK, identity, machine
from managed_system_vsock import Platform, wait_for

PHASES = ("authority_prepared", "offered", "applying", "identity_ready", "committing", "policy_armed")
TARGET_ORDINAL = 0
PROGRAM = "/run/owned-gate.pl"
RECEIPTS = "/run/owned-gate-receipts"
CONTAINER = "/run/owned-gate-container"
CONTAINER_RUNTIME = {
    "crun": "./usr/bin/crun",
    "ld-musl-x86_64.so.1": "./lib/ld-musl-x86_64.so.1",
    "libseccomp.so.2": "./usr/lib/libseccomp.so.2",
    "libcap.so.2": "./usr/lib/libcap.so.2",
    "libjson-c.so.5": "./usr/lib/libjson-c.so.5",
}
APPLICATION = r"""
use strict;
use warnings;
use IO::Select;
use IO::Socket::INET;
use POSIX qw(uname);
use Socket qw(inet_ntoa unpack_sockaddr_in);

my ($mode, @args) = @ARGV;
if ($mode eq 'collect') {
    my $listener = IO::Socket::INET->new(LocalAddr => '0.0.0.0', LocalPort => 18093, Proto => 'udp') or die $!;
    open my $out, '>>', '/run/owned-gate-receipts' or die $!;
    while (defined(my $peer = $listener->recv(my $body = '', 256))) {
        my (undef, $address) = unpack_sockaddr_in($peer);
        next unless $body =~ /\A[a-z0-9-]+\t[0-9]+\t[a-zA-Z0-9.-]+\t[0-9a-f]{32}\n\z/;
        my $record = inet_ntoa($address) . "\t" . $body;
        syswrite($out, $record) == length($record) or die $!;
    }
} elsif ($mode eq 'serve') {
    my ($sink) = @args;
    my $listener = IO::Socket::INET->new(LocalAddr => '127.0.0.1', LocalPort => 18091, Proto => 'udp') or die $!;
    my $ready = IO::Select->new($listener);
    my $sequence = 0;
    while (1) {
        my ($request, $peer) = ('pulse', undef);
        if ($ready->can_read(0.4)) {
            $peer = $listener->recv($request, 128);
            next unless defined($peer) && $request =~ /\Aowned-[0-9a-f]{24}\z/;
        }
        open my $id, '<', '/etc/machine-id' or die $!;
        chomp(my $machine = <$id>);
        close $id;
        my (undef, $hostname) = uname();
        my $body = join("\t", $request, ++$sequence, $hostname, $machine) . "\n";
        my $socket = IO::Socket::INET->new(PeerAddr => $sink, PeerPort => 18093, Proto => 'udp') or die $!;
        my $bytes = $socket->send($body);
        defined($bytes) && $bytes == length($body) or die "UDP send to $sink: $!";
        open my $last, '>', '/run/owned-gate-last-send' or die $!;
        print {$last} $body;
        close $last or die $!;
        if (defined $peer) {
            $listener->send($body, 0, $peer) or die $!;
        }
    }
} elsif ($mode eq 'request') {
    my ($token, $port) = @args;
    my $socket = IO::Socket::INET->new(PeerAddr => '127.0.0.1', PeerPort => $port // 18091, Proto => 'udp') or die $!;
    $socket->send($token) or die $!;
    IO::Select->new($socket)->can_read(4) or die "owned loopback request timed out";
    $socket->recv(my $body = '', 256);
    print $body;
} else {
    die "unknown fixture command";
}
"""
CONTAINER_APPLICATION = r"""
use strict;
use warnings;
use IO::Select;
use IO::Socket::INET;
use POSIX qw(uname);

my ($sink) = @ARGV;
my $namespace = readlink '/proc/self/ns/pid' or die $!;
die "expected OCI PID 1" unless $$ == 1;
my $listener = IO::Socket::INET->new(LocalAddr => '127.0.0.1', LocalPort => 18095, Proto => 'udp') or die $!;
my $poll = IO::Select->new($listener);
my $sequence = 0;
while (1) {
    ++$sequence;
    if ($poll->can_read(0.35)) {
        my $peer = $listener->recv(my $input = '', 64);
        next unless defined($peer) && $input =~ /\Acontainer-[0-9a-f]{24}\z/;
        open my $id, '<', '/guest-identity/machine-id' or die $!;
        chomp(my $machine = <$id>);
        close $id;
        my (undef, $hostname) = uname();
        my $socket = IO::Socket::INET->new(PeerAddr => $sink, PeerPort => 18093, Proto => 'udp') or die $!;
        my $body = join("\t", $input, $sequence, $hostname, $machine) . "\n";
        $socket->send($body) or die $!;
        $listener->send($body, 0, $peer) or die $!;
    }
    open my $status, '>', '/work/status.new' or die $!;
    print {$status} "$$ $namespace $sequence\n";
    close $status or die $!;
    rename '/work/status.new', '/work/status' or die $!;
}
"""


def catalog_reader(platform):
    db = sqlite3.connect(f"file:{platform.root / 'catalog/catalog.sqlite'}?mode=ro", uri=True, timeout=0.5)
    db.row_factory = sqlite3.Row
    return closing(db)


def child_state(db, operation, ordinal):
    return db.execute("""
        SELECT c.instance_id,c.operation_id,e.incarnation_id,e.execution_epoch,e.host_boot_id,e.pid,e.bundle_digest,
               e.pid_start_time,e.phase AS process_phase,o.phase AS operation_phase,
               a.phase,a.decision,a.revision,a.prior_revision,
               EXISTS(SELECT 1 FROM network_tap_leases n WHERE n.incarnation_id=e.incarnation_id) AS has_tap,
               (SELECT n.private_ipv4 FROM network_attachments n
                WHERE n.instance_id=c.instance_id AND n.state<>'released' LIMIT 1) AS address
        FROM managed_fork_children c
        JOIN operations o ON o.operation_id=c.operation_id
        LEFT JOIN executions e ON e.last_operation_id=c.operation_id AND e.instance_id=c.instance_id
        LEFT JOIN target_activations a ON a.operation_id=c.operation_id AND a.incarnation_id=e.incarnation_id
             AND a.instance_id=c.instance_id AND a.execution_epoch=e.execution_epoch
        WHERE c.batch_id=? AND c.ordinal=?
    """, (operation, ordinal)).fetchone()


def interrupt_target(db, operation, phase, ref, source_pid, bundle, timeout):
    current_boot = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
    samples = {}

    def observations():
        return {name: {"observed_ms": (last - first) / 1_000_000, "samples": count}
                for name, (first, last, count) in samples.items()}

    deadline = time.monotonic() + timeout
    with ExitStack() as stack:
        process = None
        key = None
        while time.monotonic() < deadline:
            row = child_state(db, operation, TARGET_ORDINAL)
            if row is None or row["pid"] is None:
                time.sleep(0.004)
                continue
            if position := row["phase"]:
                tick = time.monotonic_ns()
                first, _, count = samples.get(position, (tick, tick, 0))
                samples[position] = (first, tick, count + 1)
            if row["operation_phase"] != "NEW" or row["phase"] == "released":
                break
            if process is None:
                assert row["pid"] != source_pid and row["instance_id"] != ref["instance_id"], dict(row)
                assert row["host_boot_id"] == current_boot and row["process_phase"] == "registered", dict(row)
                assert row["bundle_digest"] == bundle, dict(row)
                pid, start = row["pid"], row["pid_start_time"]
                try:
                    process = stack.enter_context(os.fdopen(os.pidfd_open(pid), "rb", buffering=0))
                    actual_start = int(Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19])
                except (FileNotFoundError, ProcessLookupError):
                    break
                assert start == actual_start, "owned target PID was reused"
                key = (row["incarnation_id"], row["execution_epoch"], pid, start)
            if row["phase"] == phase:
                assert key == (row["incarnation_id"], row["execution_epoch"], row["pid"], row["pid_start_time"]), dict(row)
                assert row["has_tap"] and row["address"] is not None, dict(row)
                try:
                    signal.pidfd_send_signal(process.fileno(), signal.SIGKILL)
                except ProcessLookupError:
                    break
                assert select.select([process], [], [], 8)[0], "owned target did not exit"
                after = child_state(db, operation, TARGET_ORDINAL)
                assert after is not None and after["incarnation_id"] == row["incarnation_id"], after
                verified = (after["phase"] == phase and after["revision"] == row["revision"]) or (
                    after["phase"] in ("failed_held", "unknown") and after["prior_revision"] == row["revision"]
                )
                return dict(observed=sorted(samples), phase_observations=observations(), exited=True,
                            hit=verified, before=dict(row), after=dict(after))
    return dict(observed=sorted(samples), phase_observations=observations(), exited=False, hit=False)


def run_program(platform, ref, unit, *args):
    platform.guest(ref, ["/bin/sh", "-ec", 'cat > "$1"', "owned", PROGRAM], stdin=APPLICATION)
    platform.guest(ref, ["/usr/bin/systemd-run", "--unit=" + unit, "/usr/bin/perl", PROGRAM, *args])


def copy_runtime_file(platform, ref, destination, data):
    paths = []
    chunk_size = 256 * 1024
    for offset in range(0, len(data), chunk_size):
        chunk = data[offset:offset + chunk_size]
        path = f"{destination}.part-{offset}"
        result = platform.native(f"/instances/{ref['instance_id']}/guest/upload", {
            "expected": ref,
            "request": {"path": path, "data": base64.b64encode(chunk).decode("ascii"), "mode": 0o600},
        })["result"]
        assert result["bytes_written"] == len(chunk), result
        paths.append(path)
    observed = platform.guest(ref, ["/bin/sh", "-ec", """
        dest=$1
        shift
        cat "$@" > "$dest"
        chmod 0755 "$dest"
        for part in "$@"; do unlink "$part"; done
        sha256sum "$dest"
    """, "owned", destination, *paths]).split()[0]
    assert observed.decode() == hashlib.sha256(data).hexdigest(), destination


def start_container(platform, ref, sink):
    runtime, bundle, work = (f"{CONTAINER}/{name}" for name in ("runtime", "bundle", "work"))
    rootfs = bundle + "/rootfs"
    mounts = [
        {"destination": "/proc", "type": "proc", "source": "proc", "options": ["nosuid", "noexec", "nodev"]},
        {"destination": "/dev", "type": "tmpfs", "source": "tmpfs", "options": ["nosuid", "mode=755", "size=1m"]},
    ]
    for path in ("/usr", "/lib", "/lib64"):
        mounts.append({"destination": path, "type": "bind", "source": path, "options": ["rbind", "ro"]})
    mounts.extend((
        {"destination": "/guest-identity", "type": "bind", "source": "/etc", "options": ["rbind", "ro"]},
        {"destination": "/work", "type": "bind", "source": work, "options": ["rbind", "rw"]},
    ))
    platform.guest(ref, ["mkdir", "-p", runtime + "/state", work,
                         *(rootfs + mount["destination"] for mount in mounts)])
    with tarfile.open(platform.bundle / "guest/oci-rootfs.tar.gz", "r:gz") as archive:
        for name, member in CONTAINER_RUNTIME.items():
            with archive.extractfile(member) as stream:
                copy_runtime_file(platform, ref, runtime + "/" + name, stream.read())
    platform.guest(ref, ["ln", "-s", "ld-musl-x86_64.so.1", runtime + "/libc.musl-x86_64.so.1"])
    crun = [runtime + "/ld-musl-x86_64.so.1", "--library-path", runtime, runtime + "/crun"]
    assert b"crun version" in platform.guest(ref, [*crun, "--version"])
    config = {"ociVersion": "1.0.2", "root": {"path": "rootfs", "readonly": True},
              "process": {"terminal": False, "cwd": "/", "user": {"uid": 0, "gid": 0}, "noNewPrivileges": True,
                          "args": ["/usr/bin/perl", "/work/container.pl", sink], "env": ["LANG=C"]},
              "mounts": mounts, "linux": {"namespaces": [{"type": kind} for kind in ("pid", "mount", "ipc")]}}
    for path, body in ((bundle + "/config.json", json.dumps(config)), (work + "/container.pl", CONTAINER_APPLICATION)):
        platform.guest(ref, ["/bin/sh", "-ec", 'cat > "$1"', "owned", path], stdin=body)
    platform.guest(ref, ["/usr/bin/systemd-run", "--unit=owned-gate-container", *crun,
                         "--root", runtime + "/state", "--cgroup-manager=disabled", "run", "--bundle", bundle, "owned-gate"])


def container_status(platform, ref):
    pid, namespace, counter = platform.guest(ref, ["cat", CONTAINER + "/work/status"]).decode().split()
    assert pid == "1" and namespace.startswith("pid:["), (pid, namespace, counter)
    return namespace, int(counter)


def wait_for_container(platform, ref, previous=None):
    def advancing():
        now = container_status(platform, ref)
        if previous is None:
            return now
        assert now[0] == previous[0], (previous, now)
        return now if now[1] > previous[1] else None

    try:
        return wait_for(advancing, timeout=20, retry=(RuntimeError, OSError))
    except TimeoutError as error:
        detail = platform.guest(ref, ["journalctl", "-u", "owned-gate-container.service", "-n", "20", "--no-pager"])
        raise RuntimeError(detail.decode(errors="replace")) from error


def container_message(platform, ref, ip, expected, rows):
    token = "container-" + os.urandom(12).hex()
    reply = platform.guest(ref, ["/usr/bin/perl", PROGRAM, "request", token, "18095"]).decode().strip().split("\t")
    assert reply[0] == token and tuple(reply[2:]) == expected, reply

    def received():
        matches = [entry for entry in rows() if entry[1] == token]
        if matches:
            (entry,) = matches
            assert entry == [ip, *reply], (entry, reply)
            return entry

    try:
        return wait_for(received, timeout=8)
    except TimeoutError:
        print("Owned container loopback completed without collector receipt", json.dumps([ip, *reply]), flush=True)
        raise


def endpoint(platform, mid):
    return wait_for(lambda: (row if (row := platform.app(f"machines/{mid}/network"))["state"] == "published" else None))


def guest_identity(platform, ref):
    output = platform.guest(ref, ["cat", "/proc/sys/kernel/hostname", "/etc/machine-id"]).decode().splitlines()
    assert len(output) == 2 and len(output[1]) == 32, output
    return tuple(output)


def gate_case(platform, phase, with_container, cut_timeout):
    observer, _, observer_ref = machine(platform, "collector", "native")
    collector = endpoint(platform, observer)["private_ipv4"]
    run_program(platform, observer_ref, "owned-gate-collector", "collect")
    source, _, ref = machine(platform, "source", "fixed_internal")
    source_ip = endpoint(platform, source)["private_ipv4"]
    path = f"machines/{source}"
    run_program(platform, ref, "owned-gate-workload", "serve", collector)
    old_identity = guest_identity(platform, ref)

    def rows():
        raw = platform.guest(observer_ref, ["cat", RECEIPTS]).decode()
        return [record.split("\t") for record in raw.splitlines() if record.count("\t") == 4]

    def round_trip(target, ip, expected):
        token = "owned-" + os.urandom(12).hex()
        response = platform.guest(target, ["/usr/bin/perl", PROGRAM, "request", token]).decode().strip().split("\t")
        assert response[0] == token and tuple(response[2:]) == expected, response
        receipt = [ip, *response]

        def received():
            matches = [entry for entry in rows() if entry[1] == token]
            if matches:
                assert matches == [receipt], (matches, receipt)
                return matches[0]

        try:
            return wait_for(received, timeout=8)
        except TimeoutError:
            def observe(execution, command):
                try:
                    return platform.guest(execution, command).decode(errors="replace")
                except (OSError, RuntimeError) as error:
                    return str(error)

            entries = rows()
            print("Owned loopback completed without collector receipt", json.dumps({
                "expected": receipt, "recent_from_sender": [entry for entry in entries if entry[0] == ip][-4:],
                "last_submitted": observe(target, ["cat", "/run/owned-gate-last-send"]),
                "sender_unit": observe(target, ["systemctl", "show", "owned-gate-workload",
                                                "-p", "ActiveState", "-p", "ExecMainStatus"]),
                "sender_routes": observe(target, ["ip", "-4", "route"]),
                "sender_neighbors": observe(target, ["ip", "neigh"]),
                "sender_link": observe(target, ["ip", "-s", "link", "show", "dev", "eth0"]),
                "sender_queue": observe(target, ["tc", "-s", "qdisc", "show", "dev", "eth0"]),
                "collector_unit": observe(observer_ref, ["systemctl", "show", "owned-gate-collector",
                                                         "-p", "ActiveState", "-p", "ExecMainStatus"]),
                "collector_bytes": observe(observer_ref, ["wc", "-c", RECEIPTS]),
                "collector_tail": observe(observer_ref, ["tail", "-5", RECEIPTS]),
            }), flush=True)
            raise

    wait_for(lambda: any(row[0] == source_ip for row in rows()), retry=(RuntimeError, OSError))
    before = round_trip(ref, source_ip, old_identity)
    container_proof = {}
    if with_container:
        start_container(platform, ref, collector)
        original = wait_for_container(platform, ref)
        system_namespace = platform.guest(ref, ["readlink", "/proc/1/ns/pid"]).decode().strip()
        assert original[0] != system_namespace
        first = container_message(platform, ref, source_ip, old_identity, rows)
        container_proof = {"pid_namespace": original[0], "source_before": first}
    source_pid = platform.native(f"/instances/{ref['instance_id']}/inventory")["execution"]["pid"]
    def fork_one(name, key):
        return platform.change(path + "/fork-batches", key, {
            "children": [{"name": name}], "require_independent": True,
        }, platform.wait_capability(path, "state.fork_batch"))

    accepted = fork_one("owned cut target", "owned-gate-cut")
    with catalog_reader(platform) as db:
        cut = interrupt_target(db, accepted["operation_id"], phase, ref, source_pid, platform.bundle_id, cut_timeout)
    print("Owned activation cut", json.dumps({"requested_phase": phase, "bundle_digest": platform.bundle_id, **cut}, sort_keys=True), flush=True)
    def terminal():
        result = platform.app("operations/" + accepted["operation_id"])
        if result["state"] in ("succeeded", "failed"):
            return result
        assert result["state"] not in ("cancelled", "recovery_required"), result

    finished = wait_for(terminal, timeout=120)
    assert finished["source_lock_released"] and finished["source_disposition"] == "RUNNING", finished
    _, parent_now = platform.context(source)
    assert identity(parent_now) == identity(ref)
    assert guest_identity(platform, ref) == old_identity
    parent = round_trip(ref, source_ip, old_identity)
    assert int(parent[2]) > int(before[2]), (before, parent)
    if with_container:
        wait_for_container(platform, ref, original)
        container_proof["source_after_failure"] = container_message(platform, ref, source_ip, old_identity, rows)
    if not cut["hit"]:
        print("Requested cut was not proved.", flush=True)
        return False

    with catalog_reader(platform) as db:
        failed = child_state(db, accepted["operation_id"], TARGET_ORDINAL)
        key = (cut["before"]["incarnation_id"], cut["before"]["execution_epoch"])
        leases = db.execute("""
            SELECT t.state, k.lease_id IS NOT NULL AS published FROM network_tap_leases t
            LEFT JOIN network_kernel_receipts k USING(lease_id)
            WHERE t.incarnation_id=? AND t.execution_epoch=?
        """, key).fetchall()
        grants = db.execute("SELECT state FROM writer_grants WHERE incarnation_id=? AND execution_epoch=?", key).fetchall()
        reserved = db.execute("SELECT COUNT(*) FROM writer_reservations WHERE incarnation_id=? AND execution_epoch=?", key).fetchone()[0]
    assert failed is not None, cut
    expected_scope = {field: cut["before"][field] for field in
                      ("operation_id", "instance_id", "incarnation_id", "execution_epoch")}
    for row in (cut["after"], failed):
        assert all(row[field] == value for field, value in expected_scope.items()), dict(row)
    decisions = {"before_exit": cut["before"]["decision"], "after_exit": cut["after"]["decision"], "settled": failed["decision"]}
    if phase in ("committing", "policy_armed"):
        assert decisions["before_exit"] == "commit", decisions
    if "commit" in decisions.values():
        assert decisions["settled"] == "commit", decisions
    expected_effect = "unknown" if decisions["settled"] == "commit" else "none"
    failed_child = finished["children"][TARGET_ORDINAL]
    assert finished["state"] == "failed" and finished["effect"] == expected_effect, (finished, decisions)
    assert failed_child["state"] in ("failed", "quarantined") and failed["operation_phase"] == "ABORTED", finished
    assert failed["process_phase"] == "exited" and failed["phase"] != "released", dict(failed)
    assert leases and all(lease["state"] == "retired" and not lease["published"] for lease in leases), [dict(lease) for lease in leases]
    assert grants and all(grant["state"] == "released" for grant in grants) and reserved == 0, (grants, reserved)
    visible = platform.app("machines")["items"]
    assert not any(item.get("machine_id") == failed_child.get("machine_id") for item in visible), visible
    cut_ip = str(ipaddress.IPv4Address(cut["before"]["address"]))
    assert all(row[0] != cut_ip for row in rows()), "collector saw I/O from failed target"
    print("Owned activation settled", json.dumps({
        "phase": phase, "operation_id": accepted["operation_id"], "target": dict(failed),
        "leases": [dict(lease) for lease in leases], "grants": [dict(grant) for grant in grants],
        "source_disposition": finished["source_disposition"], "effect": finished["effect"], "activation_decisions": decisions,
    }, sort_keys=True), flush=True)

    completed = platform.complete(fork_one("owned positive target", "owned-gate-positive"))
    assert completed["source_lock_released"] and completed["source_disposition"] == "RUNNING", completed
    (child,) = completed["children"]
    assert child["state"] == "ready" and child["resource_independence"] == "verified", child
    _, target = platform.context(child["machine_id"])
    new_identity = guest_identity(platform, target)
    assert all(old != new for old, new in zip(old_identity, new_identity, strict=True)), (old_identity, new_identity)
    child_ip = endpoint(platform, child["machine_id"])["private_ipv4"]
    assert child_ip not in (source_ip, collector)
    first_child = wait_for(lambda: next((row for row in rows() if row[0] == child_ip), None), timeout=8)
    assert tuple(first_child[3:]) == new_identity and int(first_child[2]) > 1, first_child
    # A restored sender can deliver its first packets and stall after a few TX completions.
    began = time.monotonic()
    child_replies = [round_trip(target, child_ip, new_identity)]
    for _ in range(2):
        time.sleep(6)
        reply = round_trip(target, child_ip, new_identity)
        assert int(reply[2]) > int(child_replies[-1][2]), (child_replies[-1], reply)
        child_replies.append(reply)
    elapsed = time.monotonic() - began
    if with_container:
        status = container_status(platform, target)
        assert status[0] == original[0] and status[1] > original[1], (original, status)
        container_proof["child_continued_counter"] = wait_for_container(platform, target, status)[1]
        first = container_message(platform, target, child_ip, new_identity, rows)
        assert first == next(row for row in rows() if row[0] == child_ip and row[1].startswith("container-"))
        assert int(first[2]) > int(container_proof["source_before"][2]), (first, container_proof)
        container_proof["child_first_external"] = first
    assert all(row[0] != cut_ip for row in rows()), "collector saw I/O from failed target"
    print("Owned activation witness", json.dumps({
        "phase": phase, "bundle_digest": platform.bundle_id, "effect": finished["effect"],
        "operation_id": accepted["operation_id"], "positive_operation_id": completed["operation_id"],
        "source": identity(ref), "child": identity(target),
        "source_loopback": parent, "child_loopback": child_replies[0], "child_first_external": first_child,
        "child_continuity": {"duration_seconds": round(elapsed, 3), "receipts": child_replies},
        "failed_address": cut_ip, "failed_activation": failed["phase"], "failed_receipts": 0,
        **({"container": container_proof} if with_container else {}),
    }, sort_keys=True), flush=True)
    return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "platformd", "system-image"):
        parser.add_argument("--" + name, type=lambda path: Path(path).resolve(), required=True)
    parser.add_argument("--phase", choices=PHASES, required=True)
    parser.add_argument("--cut-timeout", type=int, default=45, help="seconds to observe the selected cut (default: 45)")
    parser.add_argument("--use-installation", action="store_true")
    parser.add_argument("--container", action="store_true", help="run an owned OCI workload in the System Guest")
    args = parser.parse_args()
    if args.cut_timeout < 1:
        parser.error("--cut-timeout must be positive")
    if os.geteuid() != 0:
        parser.error("local KVM requires an isolated privileged fixture")
    if os.environ.get("OWNED_GATE_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, "-B", __file__, *sys.argv[1:]],
                   {**os.environ, "OWNED_GATE_NAMESPACE": "1", "PYTHONDONTWRITEBYTECODE": "1"})
    with tempfile.TemporaryDirectory(dir="/var/tmp", prefix="owned-gate-") as directory, ExitStack() as stack:
        root = Path(directory)
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=8G,mode=0755,nosuid,nodev", "tmpfs", root], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", root], check=True)
        slice_name = f"smolvmownedgate{os.getpid()}_{os.urandom(4).hex()}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        slice_name, "fail", "1", "MemoryAccounting", "b", "true", "0"],
                       check=True, stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", slice_name], check=True)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = slice_name
        platform = Platform(args, root, stack, (NETWORK,), use_installation=args.use_installation)
        try:
            platform.start()
            succeeded = gate_case(platform, args.phase, args.container, args.cut_timeout)
            peak = Path("/sys/fs/cgroup") / slice_name / "memory.peak"
            if peak.is_file():
                print("Owned fixture slice memory peak", peak.read_text().strip(), flush=True)
            return 0 if succeeded else 2
        except BaseException:
            for name in ("platform.log", "worker.log", "serve.log"):
                log = root / name
                if log.exists():
                    print(name, log.read_text(errors="replace")[-1600:], file=sys.stderr)
            raise


if __name__ == "__main__":
    sys.exit(main())
