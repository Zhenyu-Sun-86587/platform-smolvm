#!/usr/bin/env python3
"""Verify an owned System target's activation interruption with real KVM I/O."""

import argparse
from contextlib import ExitStack, closing
import ipaddress
import json
import os
from pathlib import Path
import select
import signal
import sqlite3
import subprocess
import sys
import tempfile
import time

from authorized_services_kvm import NETWORK, identity, machine
from managed_system_vsock import Platform, wait_for

PHASES = ("authority_prepared", "offered", "applying", "identity_ready", "committing", "policy_armed")
TARGET_ORDINAL = 1
PROGRAM = "/run/owned-gate.pl"
RECEIPTS = "/run/owned-gate-receipts"
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
        $socket->send($body);
        if (defined $peer) {
            $listener->send($body, 0, $peer) or die $!;
        }
    }
} elsif ($mode eq 'request') {
    my ($token) = @args;
    my $socket = IO::Socket::INET->new(PeerAddr => '127.0.0.1', PeerPort => 18091, Proto => 'udp') or die $!;
    $socket->send($token) or die $!;
    IO::Select->new($socket)->can_read(4) or die "owned loopback request timed out";
    $socket->recv(my $body = '', 256);
    print $body;
} else {
    die "unknown fixture command";
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
        WHERE c.batch_id=? AND c.ordinal=?
    """, (operation, ordinal)).fetchone()


def interrupt_target(db, operation, phase, ref, source_pid, bundle):
    current_boot = Path("/proc/sys/kernel/random/boot_id").read_text().strip()
    seen = set()
    deadline = time.monotonic() + 45
    with ExitStack() as stack:
        process = None
        key = None
        while time.monotonic() < deadline:
            row = child_state(db, operation, TARGET_ORDINAL)
            if row is None or row["pid"] is None:
                time.sleep(0.004)
                continue
            if row["phase"]:
                seen.add(row["phase"])
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
                return dict(observed=sorted(seen), exited=True, hit=verified, before=dict(row), after=dict(after))
    return dict(observed=sorted(seen), exited=False, hit=False)


def run_program(platform, ref, unit, *args):
    platform.guest(ref, ["/bin/sh", "-ec", 'cat > "$1"', "owned", PROGRAM], stdin=APPLICATION)
    platform.guest(ref, ["/usr/bin/systemd-run", "--unit=" + unit, "/usr/bin/perl", PROGRAM, *args])


def endpoint(platform, mid):
    return wait_for(lambda: (row if (row := platform.app(f"machines/{mid}/network"))["state"] == "published" else None))


def guest_identity(platform, ref):
    output = platform.guest(ref, ["cat", "/proc/sys/kernel/hostname", "/etc/machine-id"]).decode().splitlines()
    assert len(output) == 2 and len(output[1]) == 32, output
    return tuple(output)


def gate_case(platform, phase):
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
        wait_for(lambda: receipt in rows(), timeout=8)
        return receipt

    wait_for(lambda: any(row[0] == source_ip for row in rows()), retry=(RuntimeError, OSError))
    before = round_trip(ref, source_ip, old_identity)
    source_pid = platform.native(f"/instances/{ref['instance_id']}/inventory")["execution"]["pid"]
    accepted = platform.change(path + "/fork-batches", "owned-gate-cut", {
        "children": [{"name": "owned surviving child"}, {"name": "owned cut target"}],
        "require_independent": True,
    }, platform.wait_capability(path, "state.fork_batch"))
    with catalog_reader(platform) as db:
        cut = interrupt_target(db, accepted["operation_id"], phase, ref, source_pid, platform.bundle_id)
    print("Owned activation cut", json.dumps({"requested_phase": phase, "bundle_digest": platform.bundle_id, **cut}, sort_keys=True), flush=True)
    finished = platform.complete(accepted)
    assert finished["source_lock_released"] and finished["source_disposition"] == "RUNNING", finished
    _, parent_now = platform.context(source)
    assert identity(parent_now) == identity(ref)
    assert guest_identity(platform, ref) == old_identity
    parent = round_trip(ref, source_ip, old_identity)
    assert int(parent[2]) > int(before[2]), (before, parent)
    child = finished["children"][0]
    assert child["state"] == "ready" and child["resource_independence"] == "verified", child
    _, target = platform.context(child["machine_id"])
    new_identity = guest_identity(platform, target)
    assert all(old != new for old, new in zip(old_identity, new_identity, strict=True)), (old_identity, new_identity)
    child_ip = endpoint(platform, child["machine_id"])["private_ipv4"]
    assert child_ip not in (source_ip, collector)
    received = round_trip(target, child_ip, new_identity)
    first_child = next(row for row in rows() if row[0] == child_ip)
    assert tuple(first_child[3:]) == new_identity and int(first_child[2]) > 1, first_child
    if not cut["hit"]:
        print("Requested cut was not proved. The owned sibling completed ordinary loopback and private I/O.", flush=True)
        return False

    with catalog_reader(platform) as db:
        failed = child_state(db, accepted["operation_id"], TARGET_ORDINAL)
        leases = db.execute("SELECT state FROM network_tap_leases WHERE incarnation_id=?", (
            cut["before"]["incarnation_id"],
        )).fetchall()
    failed_child = finished["children"][TARGET_ORDINAL]
    assert failed_child["state"] == "failed" and failed["operation_phase"] == "ABORTED", finished
    assert failed["process_phase"] == "exited" and failed["phase"] != "released", dict(failed)
    assert all(lease["state"] == "retired" for lease in leases), [dict(lease) for lease in leases]
    visible = platform.app("machines")["items"]
    assert not any(item.get("machine_id") == failed_child.get("machine_id") for item in visible), visible
    cut_ip = str(ipaddress.IPv4Address(cut["before"]["address"]))
    assert all(row[0] != cut_ip for row in rows()), "collector saw I/O from failed target"
    print("Owned activation witness", json.dumps({
        "phase": phase, "bundle_digest": platform.bundle_id, "effect": finished["effect"],
        "operation_id": accepted["operation_id"], "source": identity(ref), "child": identity(target),
        "source_loopback": parent, "child_loopback": received, "child_first_external": first_child,
        "failed_address": cut_ip, "failed_activation": failed["phase"], "failed_receipts": 0,
    }, sort_keys=True), flush=True)
    return True


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "platformd", "system-image"):
        parser.add_argument("--" + name, type=lambda path: Path(path).resolve(), required=True)
    parser.add_argument("--phase", choices=PHASES, required=True)
    parser.add_argument("--use-installation", action="store_true")
    args = parser.parse_args()
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
            return 0 if gate_case(platform, args.phase) else 2
        except BaseException:
            for name in ("platform.log", "worker.log", "serve.log"):
                log = root / name
                if log.exists():
                    print(name, log.read_text(errors="replace")[-1600:], file=sys.stderr)
            raise


if __name__ == "__main__":
    sys.exit(main())
