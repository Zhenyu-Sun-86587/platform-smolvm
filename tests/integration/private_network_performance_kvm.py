#!/usr/bin/env python3
"""Measure bounded TCP exchanges over an owned pair of Worker private interfaces."""

import argparse
from contextlib import ExitStack
import json
import math
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile
import time

from authorized_private_network_kvm import CIDR, launch
from authorized_services_kvm import identity, witness
from managed_system_vsock import Platform, digest, wait_for


PORT = 18133
CGROUP = Path("/sys/fs/cgroup")


def vmm_identity(platform, endpoint):
    ref = endpoint["execution"]
    current = platform.native(f"/instances/{ref['instance_id']}/inventory")["execution"]
    assert identity(current) == identity(ref), current
    pid = current["pid"]
    path = Path(f"/proc/{pid}")
    scope, = [line.removeprefix("0::/") for line in (path / "cgroup").read_text().splitlines()
              if line.startswith("0::/")]
    assert scope.endswith(".scope") and (CGROUP / scope / "cpu.stat").is_file(), scope
    return {"pid": pid, "start_time": current["pid_start_time"], "scope": scope}


def cpu_usec(machine):
    identity_file = Path(f"/proc/{machine['pid']}/stat")
    observed = int(identity_file.read_text().rsplit(")", 1)[1].split()[19])
    assert observed == machine["start_time"], machine
    stats = dict(line.split() for line in (CGROUP / machine["scope"] / "cpu.stat").read_text().splitlines())
    return int(stats["usage_usec"])


def packet_counts(platform, endpoint):
    path = f"/sys/class/net/{endpoint['interface']}/statistics"
    raw = platform.guest(endpoint["execution"], ["cat", path + "/rx_packets", path + "/tx_packets"])
    receive, send = map(int, raw.splitlines())
    return {"rx": receive, "tx": send}


def start_probe(platform, endpoint):
    ref = endpoint["execution"]
    addresses = json.loads(platform.guest(ref, ["ip", "-4", "-j", "address", "show", "scope", "global"]))
    address = endpoint["endpoint"]["guest_ipv4"]
    interface, = [link["ifname"] for link in addresses if any(addr["local"] == address for addr in link["addr_info"])]
    endpoint["interface"] = interface
    endpoint["probe"] = platform.place_probe(ref, platform.args.probe)
    platform.guest(ref, ["systemd-run", "--unit=wp22-private-network-performance", endpoint["probe"], "server", str(PORT)])
    wait_for(lambda: f":{PORT:04X}".encode() in platform.guest(ref, ["cat", "/proc/net/tcp"]))
    endpoint["vmm"] = vmm_identity(platform, endpoint)


def nearest_rank(samples, fraction):
    return sorted(samples)[math.ceil(len(samples) * fraction) - 1]


def measure(platform, source, target, kind):
    vmm = {"sender": source["vmm"], "receiver": target["vmm"]}
    packets_before = packet_counts(platform, target)
    load_before = os.getloadavg()
    started = time.monotonic_ns()
    cpu_before = {name: cpu_usec(machine) for name, machine in vmm.items()}
    answer = json.loads(platform.guest(source["execution"], [
        source["probe"], kind, str(PORT), target["endpoint"]["private_ipv4"], source["endpoint"]["private_ipv4"],
    ]))
    cpu = {name: cpu_usec(machine) - cpu_before[name] for name, machine in vmm.items()}
    elapsed = time.monotonic_ns() - started
    load_after = os.getloadavg()
    packets_after = packet_counts(platform, target)
    packets = {name: packets_after[name] - packets_before[name] for name in packets_before}
    assert all(value > 0 for value in packets.values()), packets
    assert all(value > 0 for value in cpu.values()), cpu
    if kind == "rtt":
        samples = answer["rtt_ns"]
        assert len(samples) == 48 and all(sample > 0 for sample in samples), answer
        answer["p50_ns"] = nearest_rank(samples, 0.5)
        answer["p95_ns"] = nearest_rank(samples, 0.95)
    else:
        transfers = answer["transfers"]
        assert [transfer["sequence"] for transfer in transfers] == [0, 1, 2], answer
        assert all(transfer["verified"] and transfer["bytes"] == (4 << 20) and transfer["elapsed_ns"] > 0
                   for transfer in transfers), answer
        sent_mib = sum(transfer["bytes"] for transfer in transfers) / (1 << 20)
        seconds = sum(transfer["elapsed_ns"] for transfer in transfers) / 1e9
        answer["payload_mib_per_second"] = sent_mib / seconds
    return dict(case=kind, raw=answer, host_elapsed_ns=elapsed, receiver_packets=packets,
                vmm_cpu_usec=cpu, vmm_cpu_usec_per_packet=sum(cpu.values()) / sum(packets.values()),
                host_loadavg_before=load_before, host_loadavg_after=load_after)


def run(platform, network, mode):
    first, second = [launch(platform, label, network, mode) for label in ("alpha", "beta")]
    for endpoint in (first, second):
        start_probe(platform, endpoint)
    assert first["vmm"]["scope"] != second["vmm"]["scope"]
    assert first["mtu"] == second["mtu"] == 1500
    private = [endpoint["endpoint"]["private_ipv4"] for endpoint in (first, second)]
    assert len(set(private)) == 2
    directions = []
    for source, target in ((first, second), (second, first)):
        directions.append(dict(source=source["endpoint"]["private_ipv4"], target=target["endpoint"]["private_ipv4"],
                               target_interface=target["interface"],
                               windows=[measure(platform, source, target, kind) for kind in ("rtt", "bulk")]))
    for endpoint in (first, second):
        assert vmm_identity(platform, endpoint) == endpoint["vmm"]
    identity_info = [dict(execution=identity(endpoint["execution"]), vmm=endpoint["vmm"],
                          guest_ipv4=endpoint["endpoint"]["guest_ipv4"], private_ipv4=endpoint["endpoint"]["private_ipv4"])
                     for endpoint in (first, second)]
    cpu_model = next(line.partition(":")[2].strip() for line in Path("/proc/cpuinfo").read_text().splitlines()
                     if line.startswith("model name"))
    virtualization = subprocess.run(["systemd-detect-virt", "--vm"], text=True, capture_output=True).stdout.strip()
    worker_host = dict(available_vcpus=len(os.sched_getaffinity(0)), kernel=os.uname().release, machine=os.uname().machine,
                       cpu_model=cpu_model, virtualization=virtualization)
    witness(platform, "wp22-owned-network-performance", mode=mode, mtu=1500, worker_host=worker_host,
            platformd=digest(platform.args.platformd), system_image=digest(platform.args.system_image), probe=digest(platform.args.probe),
            endpoints=identity_info, directions=directions)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for key in ("bundle", "platformd", "system-image", "probe"):
        parser.add_argument("--" + key, type=lambda path: Path(path).resolve(strict=True), required=True)
    parser.add_argument("--network-mode", choices=("native", "fixed_internal"), required=True)
    parser.add_argument("--work-dir", type=Path, default=Path("/tmp"))
    args = parser.parse_args()
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("requires root and KVM")
    if os.environ.get("WP22_PRIVATE_NETWORK_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, "-B", __file__, *sys.argv[1:]],
                   {**os.environ, "WP22_PRIVATE_NETWORK_NAMESPACE": "1", "PYTHONDONTWRITEBYTECODE": "1"})
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="wp22-network-") as directory, ExitStack() as stack:
        root = Path(directory)
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=4G,mode=0755,nosuid,nodev", "tmpfs", root], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", root], check=True)
        unit = f"smolvmperformancenet{os.getpid()}_{secrets.token_hex(4)}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        unit, "fail", "1", "MemoryAccounting", "b", "true", "0"],
                       check=True, stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", unit], check=True)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = unit
        network = dict(network_id="wp22-owned", route_domain_id="wp22-owned", cidr=CIDR)
        platform = Platform(args, root, stack, (network,), state_tmpfs=True, use_installation=True)
        try:
            platform.start()
            run(platform, network, args.network_mode)
        except BaseException:
            for log in root.glob("*.log"):
                print(log.name, log.read_text(errors="replace")[-2000:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
