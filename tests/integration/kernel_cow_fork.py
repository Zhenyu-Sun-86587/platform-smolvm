"""Public System Fork qualification using a live process, two disks and a private network."""

import json
import re
from pathlib import Path
import time

from managed_system_vsock import attach_for_retirement, wait_for

# The nonce and 16 MiB allocation live only in the process. A cold restart gets a new nonce.
SERVER = r"""
use strict;
use IO::Socket::INET;
my $listen = IO::Socket::INET->new(LocalPort=>22423, Listen=>4, ReuseAddr=>1) or die $!;
open my $entropy, '<', '/dev/urandom' or die $!;
read($entropy, my $secret, 24) == 24 or die 'short read';
my $nonce = unpack('H*', $secret);
my $memory = 'A' x (16 * 1024 * 1024);
while (my $client = $listen->accept()) {
    my $op = <$client> // ''; chomp $op;
    if ($op =~ /^set ([A-Z])$/) { substr($memory, -1, 1) = $1; }
    print {$client} join(' ', $$, $nonce, length($memory), substr($memory,-1)), "\n";
    close $client;
}
"""
CLIENT = r"""
use IO::Socket::INET;
my $c=IO::Socket::INET->new(PeerAddr=>$ARGV[0],PeerPort=>22423,Timeout=>3) or die $!;
print {$c} "$ARGV[1]\n";
my $reply=<$c>; defined $reply or die 'no reply'; print $reply;
"""


def read_workload(platform, ref, command="get", ip="127.0.0.1"):
    return platform.guest(ref, ["perl", "-e", CLIENT, ip, command]).decode().strip().split()


def vmm_memory(platform, ref):
    inv = platform.native(f"/instances/{ref['instance_id']}/inventory")
    execution = inv["execution"]
    assert all(execution[k] == v for k, v in ref.items()), (execution, ref)
    proc = Path(f"/proc/{execution['pid']}")
    def start_ticks():
        return int((proc / "stat").read_text().rsplit(")", 1)[1].split()[19])
    assert start_ticks() == execution["pid_start_time"]
    result = {"pid": execution["pid"], "incarnation_id": ref["incarnation_id"]}
    for file, names in (("status", {"VmPTE"}), ("smaps_rollup", {"Rss", "Pss"})):
        for line in (proc / file).read_text().splitlines():
            key, _, value = line.partition(":")
            if key in names:
                number, unit = value.split()
                assert unit == "kB"
                result[key + "_kib"] = int(number)
    assert start_ticks() == execution["pid_start_time"]
    return result


def write_disk(platform, ref, marker):
    platform.guest(ref, ["sh", "-ec",
        'printf %s "$1" > /root/cow-value; printf %s "$1" | dd of=/dev/vdb conv=fsync,notrunc status=none; sync',
        "fixture", marker])


def read_disk(platform, ref):
    return platform.guest(ref, ["sh", "-ec", "cat /root/cow-value; dd if=/dev/vdb bs=1 count=4 status=none"])


def run(platform, intent, case):
    if case == "cow-restore-reboot":
        proof = json.loads((platform.root / "reboot-proof.json").read_text())
        assert not Path("/sys/kernel/smolvm_cow/abi").exists()
        assert proof["boot_id"] != Path("/proc/sys/kernel/random/boot_id").read_text().strip()
        assert platform.bundle_id == proof["bundle"]
        mid = proof["machine_id"]
        path = "machines/" + mid
        saved = platform.app(path)
        assert saved["power_state"] == "hibernated", saved
        platform.complete(platform.change(path + "/start", "stock-wake", {"expected_power_state": "hibernated"}, saved))
        view, ref = platform.context(mid)
        assert ref["incarnation_id"] != proof["source"]["incarnation_id"]
        assert wait_for(lambda: read_workload(platform, ref), retry=(RuntimeError, OSError)) == proof["response"]
        assert read_disk(platform, ref) == b"origorig"
        cap = platform.wait_capability(path, "state.independent_fork")["capabilities"]["state.independent_fork"]
        assert cap["memory_fork"]["native_supported"] and not cap["memory_fork"]["kernel_cow_supported"], cap
        print("COW_STOCK_RESTORE " + json.dumps(dict(before=proof["source"], after=ref, machine_id=mid,
              bundle=platform.bundle_id, hibernate_operation=proof["hibernate_operation"])), flush=True)
        return
    backend = {"cow-native": "native", "cow-legacy": "auto", "cow-auto": "auto"}.get(case, "kernel_cow")
    expected = "native" if case in ("cow-native", "cow-legacy") else "kernel_cow"
    intent.update(start=False, resources=dict(cpu_count=1, memory_mib=512),
                  network=dict(network_id="cow-test", mode="native"),
                  lifecycle_policy=dict(keep_running=True, auto_standby=False, auto_hibernate=False))
    if expected == "kernel_cow":
        intent["memory_fork_profile"] = "kernel_cow_anon_v1"
    mid = platform.complete(platform.change("machines", "create", intent))["machine_id"]
    source = "machines/" + mid
    volume = platform.complete(platform.change("volumes", "cow-data",
        dict(name="cow-data", runtime_instance_id=platform.serve_id, capacity_bytes=8 << 20)))["result"]["volume"]
    attach_for_retirement(platform, source, volume, "cow")
    _, ref = platform.context(mid)
    wait_for(lambda: platform.guest(ref, ["cat", "/proc/1/comm"]) == b"systemd\n", retry=(RuntimeError, OSError))
    platform.guest(ref, ["sh", "-ec", 'printf %s "$1" > /run/cow-ram.pl', "fixture", SERVER])
    platform.guest(ref, ["systemd-run", "--unit=cow-ram", "/usr/bin/perl", "/run/cow-ram.pl"])
    try:
        original = wait_for(lambda: read_workload(platform, ref), timeout=15, retry=(RuntimeError, OSError))
    except TimeoutError:
        print(platform.guest(ref, ["journalctl", "-u", "cow-ram", "-n", "20", "--no-pager"]).decode(), flush=True)
        raise
    assert len(original) == 4 and len(original[1]) == 48 and original[2:] == [str(16 << 20), "A"], original
    write_disk(platform, ref, "orig")
    view = platform.wait_capability(source, "state.independent_fork")
    observation = view["capabilities"]["state.independent_fork"].get("memory_fork")
    if case == "cow-legacy":
        assert observation is None, observation
    else:
        assert observation["kernel_cow_supported"] == (expected == "kernel_cow"), observation
    if expected == "native":
        refusal = platform.change(source + "/forks", "unavailable-kernel",
            dict(child_name="Denied", require_independent=True, memory_fork_backend="kernel_cow"), view, status=409)
        assert refusal["error"]["code"] == "CAPABILITY_UNSUPPORTED", refusal
    if case == "cow-prepare-reboot":
        assert Path("/sys/kernel/smolvm_cow/abi").is_file()
        op = platform.complete(platform.change(source + "/hibernate", "canary-hibernate", {}, platform.app(source)))
        assert platform.app(source)["power_state"] == "hibernated"
        proof = dict(boot_id=Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
                     machine_id=mid, bundle=platform.bundle_id, response=original, source=ref, hibernate_operation=op["operation_id"])
        (platform.root / "reboot-proof.json").write_text(json.dumps(proof))
        print("COW_REBOOT_PREPARED " + op["operation_id"], flush=True)
        return
    if case == "cow-lifecycle":
        from polish_product import check_capture
        check_capture(platform, source, ref)
        platform.complete(platform.change(source + "/hibernate", "cow-hibernate", {}, platform.app(source)))
        saved = platform.app(source)
        assert saved["power_state"] == "hibernated", saved
        platform.complete(platform.change(source + "/start", "cow-wake", {"expected_power_state":"hibernated"}, saved))
        _, resumed = platform.context(mid)
        assert resumed != ref
        assert wait_for(lambda: read_workload(platform, resumed), retry=(RuntimeError, OSError)) == original
        assert read_disk(platform, resumed) == b"origorig"
        print("COW_LIFECYCLE " + json.dumps(dict(bundle=platform.bundle_id, before=ref, after=resumed)), flush=True)
        return
    before = vmm_memory(platform, ref)
    body = dict(child_name="Memory child", require_independent=True, memory_fork_backend=backend)
    start = time.monotonic_ns()
    accepted = platform.change(source + "/forks", "cow-fork-once", body, view)
    accepted_at = time.monotonic_ns()
    completed = platform.complete(accepted)
    completed_at = time.monotonic_ns()
    result = completed["result"]
    assert result["selected_memory_fork_backend"] == expected and result["requested_memory_fork_backend"] == backend, result
    assert completed["source_lock_released"] and completed["source_disposition"] == "RUNNING", completed
    child, = completed["children"]
    assert child["state"] == "ready" and child["resource_independence"] == "verified", child
    path = "machines/" + child["machine_id"]
    _, target = platform.context(child["machine_id"])
    assert target != ref and target["incarnation_id"] != ref["incarnation_id"]
    assert wait_for(lambda: read_workload(platform, target), retry=(RuntimeError, OSError)) == original
    ready = time.monotonic_ns()
    replay = platform.change(source + "/forks", "cow-fork-once", body, view)
    assert replay["operation_id"] == accepted["operation_id"]
    assert read_disk(platform, target) == b"origorig"
    assert read_workload(platform, ref, "set P") == original[:3] + ["P"]
    write_disk(platform, ref, "par1")
    assert read_workload(platform, target) == original and read_disk(platform, target) == b"origorig"
    assert read_workload(platform, target, "set C") == original[:3] + ["C"]
    write_disk(platform, target, "kid1")
    addresses = [platform.app(p + "/network") for p in (source, path)]
    assert all(v["state"] == "published" for v in addresses)
    assert addresses[0]["private_ipv4"] != addresses[1]["private_ipv4"]
    assert read_workload(platform, ref, ip=addresses[1]["private_ipv4"]) == original[:3] + ["C"]
    assert read_workload(platform, target, ip=addresses[0]["private_ipv4"]) == original[:3] + ["P"]
    assert read_disk(platform, ref) == b"par1par1" and read_disk(platform, target) == b"kid1kid1"
    phases = []
    for line in (platform.root / "worker.log").read_text(errors="replace").splitlines():
        if f"capture={result['capture_id']} " not in line:
            continue
        match = re.search(r"phase=(\w+) monotonic_us=(\d+) duration_us=(\d+)", line)
        if match:
            phase, ended, duration = match.groups()
            phases.append({"phase": phase, "ended_us": int(ended), "duration_us": int(duration)})
    print("COW_WORKLOAD " + json.dumps(dict(kernel=Path("/proc/sys/kernel/osrelease").read_text().strip(),
        bundle=platform.bundle_id, operation=accepted["operation_id"], capture=result["capture_id"], backend=backend,
        selected=expected, source_before=before, source_after=vmm_memory(platform, ref), child=vmm_memory(platform,target),
        accept_ms=(accepted_at-start)/1e6, complete_ms=(completed_at-start)/1e6, first_workload_response_ms=(ready-start)/1e6,
        network=addresses, measured_worker_phases=phases)), flush=True)
