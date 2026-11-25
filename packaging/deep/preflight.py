#!/usr/bin/env python3
"""Check a deep Worker isolation inventory and inspect the local host read-only."""

import argparse
from dataclasses import dataclass
import ipaddress
import json
import os
from pathlib import Path
import re
import shlex
import stat
import subprocess


REQUIRED_PATHS = {
    "runtime_bundles", "platform_releases", "worker_state", "serve_state",
    "platform_state", "platform_config", "runtime_sockets", "build_work",
}
RESERVED_NETWORKS = {
    "PVE management": "10.10.0.0/24",
    "Tailscale": "100.64.0.0/10",
    "Mihomo fake-IP": "198.18.0.0/15",
}
UNIT = re.compile(r"[A-Za-z0-9_.@\\:\-]+\.(service|socket|slice|scope)")
SLICE = re.compile(r"[A-Za-z0-9_.\\:\-]+\.slice")


def filesystem_mounts():
    def unescape(value):
        return re.sub(r"\\([0-7]{3})", lambda m: chr(int(m[1], 8)), value)

    mounts = []
    for line in Path("/proc/self/mountinfo").read_text().splitlines():
        _, _, device, root, mount, *_ = line.split()
        mounts.append((Path(unescape(mount)), device, Path(unescape(root))))
    return sorted(mounts, key=lambda entry: len(entry[0].parts), reverse=True)


@dataclass
class InspectedPath:
    canonical: Path
    endpoint_inode: tuple[int, int] | None
    ancestor_inodes: set[tuple[int, int]]
    nearest_existing: Path
    mount_path: tuple[str, Path] | None

    @classmethod
    def from_path(cls, raw, mounts):
        path = Path(raw)
        if not path.is_absolute() or path == Path("/") or ".." in path.parts:
            raise ValueError(f"expected an absolute non-root path without '..', got {raw!r}")
        canonical = path.resolve()
        if canonical == Path("/"):
            raise ValueError(f"{raw!r} resolves to the filesystem root")
        inodes = {}
        for parent in (canonical, *canonical.parents):
            try:
                info = parent.stat()
            except FileNotFoundError:
                continue
            inodes[parent] = (info.st_dev, info.st_ino)
        mount_path = None
        for mount, device, root in mounts:
            if canonical.is_relative_to(mount):
                mount_path = (device, root / canonical.relative_to(mount))
                break
        return cls(canonical, inodes.get(canonical), set(inodes.values()), next(iter(inodes)), mount_path)

    def overlaps(self, other):
        if path_overlap(self.canonical, other.canonical):
            return True
        if self.endpoint_inode in other.ancestor_inodes or other.endpoint_inode in self.ancestor_inodes:
            return True
        if self.mount_path and other.mount_path:
            device, path = self.mount_path
            other_device, other_path = other.mount_path
            return device == other_device and path_overlap(path, other_path)
        return False


def path_overlap(left, right):
    return left.is_relative_to(right) or right.is_relative_to(left)


def parse_address(value):
    if value == "*":
        return None
    address = ipaddress.ip_address(value.split("%", 1)[0])
    if isinstance(address, ipaddress.IPv6Address) and address.ipv4_mapped:
        return address.ipv4_mapped
    return address


def parse_pool(value):
    if not isinstance(value, str) or "/" not in value:
        raise ValueError(f"expected an address pool in CIDR notation, got {value!r}")
    return ipaddress.ip_network(value)


@dataclass
class Port:
    protocol: str
    address: ipaddress.IPv4Address | ipaddress.IPv6Address | None
    first: int
    last: int

    @classmethod
    def from_range(cls, protocol, bind, first, last):
        if protocol not in ("tcp", "udp"):
            raise ValueError(f"unrecognized port protocol {protocol!r}")
        if type(first) is not int or type(last) is not int or not 1 <= first <= last <= 65535:
            raise ValueError(f"invalid port range {first!r}–{last!r}")
        return cls(protocol, parse_address(bind), first, last)

    def overlaps(self, other):
        if self.protocol != other.protocol or max(self.first, other.first) > min(self.last, other.last):
            return False
        if self.address is None or other.address is None or self.address == other.address:
            return True
        for address, peer in ((self.address, other.address), (other.address, self.address)):
            # A Linux IPv6 wildcard can also bind the IPv4 space.
            if address.is_unspecified and (address.version == 6 or address.version == peer.version):
                return True
        return False

    def __str__(self):
        bind = self.address if self.address is not None else "*"
        return f"{self.protocol} [{bind}] ports {self.first}–{self.last}"


def units_overlap(left, right):
    if left == right:
        return True
    for template, name in ((left, right), (right, left)):
        if "@." in template:
            base, kind = template.split("@.", 1)
            if name.startswith(base + "@") and name.endswith("." + kind):
                return True
    return False


def slices_overlap(left, right):
    left, right = left.removesuffix(".slice"), right.removesuffix(".slice")
    return left == right or "-" in (left, right) or left.startswith(right + "-") or right.startswith(left + "-")


@dataclass
class Route:
    route_id: str
    paths: dict[str, InspectedPath]
    units: list[str]
    slices: list[str]
    ports: list[Port]
    networks: list[ipaddress.IPv4Network | ipaddress.IPv6Network]

    @classmethod
    def from_config(cls, config, mounts):
        route_id = config["route_id"]
        present = config["present"]
        if type(present) is not bool:
            raise ValueError(f"{route_id}: expected a boolean present field")
        if not present:
            if route_id == "deep" or set(config) != {"route_id", "present"}:
                raise ValueError(f"{route_id}: absent routes cannot claim reservations")
            return cls(route_id, {}, [], [], [], [])
        paths = {name: InspectedPath.from_path(value, mounts) for name, value in config["paths"].items()}
        units, slices = config["units"], config["slices"]
        if not all(UNIT.fullmatch(unit) for unit in units) or not all(SLICE.fullmatch(name) for name in slices):
            raise ValueError(f"{route_id}: invalid systemd unit or slice name")
        ports = [Port.from_range(port["protocol"], port["bind"], *port["range"]) for port in config["ports"]]
        networks = [parse_pool(pool) for pool in config["address_pools"]]
        if route_id == "deep":
            missing = REQUIRED_PATHS - paths.keys()
            if missing:
                raise ValueError(f"deep: missing isolation paths {', '.join(sorted(missing))}")
            if not units or not slices or not ports or not networks:
                raise ValueError("deep requires explicit service, slice, listener and pool reservations")
        return cls(route_id, paths, units, slices, ports, networks)


def load_inventory(config, mounts):
    if config["schema"] != "deep-isolation/v1":
        raise ValueError("expected deep-isolation/v1")
    routes = {}
    for raw in config["routes"]:
        route = Route.from_config(raw, mounts)
        if route.route_id in routes:
            raise ValueError(f"duplicate route {route.route_id!r}")
        routes[route.route_id] = route
    required = {"deep", "official", "cocoon"}
    if required - routes.keys():
        raise ValueError(f"missing route declarations {', '.join(sorted(required - routes.keys()))}")
    networks = {name: ipaddress.ip_network(pool) for name, pool in RESERVED_NETWORKS.items()}
    for name, pool in config["host_address_reservations"].items():
        if name in networks:
            raise ValueError(f"{name}: Worker inventory cannot overwrite a standard reservation")
        networks[name] = parse_pool(pool)
    return routes, networks


def directory_conflicts(deep, other):
    for role, candidate in deep.paths.items():
        for peer_role, peer in other.paths.items():
            if candidate.overlaps(peer):
                yield f"deep {role} {candidate.canonical} overlaps {other.route_id} {peer_role} {peer.canonical}"


def resource_conflicts(deep, other):
    for unit in deep.units:
        for peer in other.units:
            if units_overlap(unit, peer):
                yield f"deep unit {unit} conflicts with {other.route_id} unit {peer}"
    for name in deep.slices:
        for peer in other.slices:
            if slices_overlap(name, peer):
                yield f"deep slice {name} conflicts with {other.route_id} slice {peer}"
    for port in deep.ports:
        for peer in other.ports:
            if port.overlaps(peer):
                yield f"deep {port} conflicts with {other.route_id} {peer}"


def network_conflicts(deep, networks):
    networks = tuple(networks)
    for pool in deep.networks:
        for name, peer in networks:
            if pool.version == peer.version and pool.overlaps(peer):
                yield f"deep pool {pool} overlaps {name} {peer}"


def read_command(*command):
    result = subprocess.run(command, capture_output=True, check=False, text=True, timeout=10,
                            env={**os.environ, "LC_ALL": "C", "SYSTEMD_COLORS": "0"})
    if result.returncode or result.stderr.strip():
        raise ValueError(f"{shlex.join(command)}: {result.stderr.strip() or result.returncode}")
    return result.stdout


def host_ports():
    ports = []
    for line in read_command("ss", "-H", "-antu").splitlines():
        protocol, _, _, _, endpoint, *_ = line.split()
        address, separator, port = endpoint.rpartition(":")
        if not separator or not address:
            raise ValueError(f"unrecognized local socket endpoint {endpoint!r}")
        if port != "*":
            number = int(port)
            if number:
                ports.append(Port.from_range(protocol, address.removeprefix("[").removesuffix("]"), number, number))
    return ports


def host_addresses():
    networks = []
    addresses = json.loads(read_command("ip", "-j", "address", "show"))
    for interface in addresses:
        for address in interface["addr_info"]:
            local, prefix = address["local"], address["prefixlen"]
            networks.append((f"host interface {interface['ifname']}", ipaddress.ip_network(f"{local}/{prefix}", strict=False)))
    return networks


def host_routes(family):
    entries = json.loads(read_command("ip", family, "-j", "route", "show", "table", "all"))
    return [(f"host route (table {entry.get('table', 'main')})", ipaddress.ip_network(entry["dst"]))
            for entry in entries if entry["dst"] != "default"]


def host_systemd():
    types = "--type=service,socket,slice,scope"
    names = set()
    for action, options in (("list-units", ("--all", "--plain")), ("list-unit-files", ())):
        output = read_command("systemctl", action, *options, types, "--no-legend", "--no-pager")
        names.update(line.split()[0] for line in output.splitlines() if line.strip())
    return sorted(names)


def host_unix_sockets(mounts):
    sockets = {}
    for line in Path("/proc/net/unix").read_text().splitlines()[1:]:
        fields = line.split(maxsplit=7)
        if len(fields) == 8 and fields[7].startswith("/"):
            sockets[fields[7]] = InspectedPath.from_path(fields[7], mounts)
    return sockets


def inspect_host(deep, mounts, conflicts, unknown):
    report = {}

    def probe(name, inspect):
        try:
            evidence = inspect()
        except (OSError, ValueError, KeyError, RuntimeError, subprocess.TimeoutExpired) as error:
            unknown.append(f"{name}: {error}")
            return None
        report[name] = len(evidence)
        return evidence

    ports = probe("bound_inet_sockets", host_ports) or []
    units = probe("systemd_units", host_systemd) or []
    networks = probe("interface_prefixes", host_addresses) or []
    networks += probe("ipv4_routes", lambda: host_routes("-4")) or []
    networks += probe("ipv6_routes", lambda: host_routes("-6")) or []
    sockets = probe("bound_unix_sockets", lambda: host_unix_sockets(mounts)) or {}
    # systemd's root slice necessarily contains all service slices. It is not reserved by a route.
    slices = [unit for unit in units if unit.endswith(".slice") and unit != "-.slice"]
    host = Route("host", sockets, units, slices, ports, [])
    conflicts.update(directory_conflicts(deep, host))
    conflicts.update(resource_conflicts(deep, host))
    conflicts.update(network_conflicts(deep, networks))

    report["architecture"] = os.uname().machine
    if report["architecture"] != "x86_64":
        unknown.append(f"Worker architecture requires x86_64, found {report['architecture']}")
    kvm = Path("/dev/kvm")
    try:
        report["kvm_accessible"] = stat.S_ISCHR(kvm.stat().st_mode) and os.access(kvm, os.R_OK | os.W_OK)
    except OSError:
        report["kvm_accessible"] = False
    if not report["kvm_accessible"]:
        unknown.append("KVM: /dev/kvm is missing or is unavailable to the current account")
    try:
        report["cgroup_v2_controllers"] = Path("/sys/fs/cgroup/cgroup.controllers").read_text().split()
        if "memory" not in report["cgroup_v2_controllers"]:
            unknown.append("cgroup v2: memory controller unavailable")
    except OSError as error:
        unknown.append(f"cgroup v2: {error}")
    try:
        report["memory_pressure"] = Path("/proc/pressure/memory").read_text().splitlines()
    except OSError as error:
        unknown.append(f"memory pressure: {error}")
    report["path_capacity"] = {}
    for name, path in deep.paths.items():
        try:
            space = os.statvfs(path.nearest_existing)
        except OSError as error:
            unknown.append(f"{name} filesystem: {error}")
            continue
        report["path_capacity"][name] = {"at": str(path.nearest_existing),
                                         "available_bytes": space.f_bavail * space.f_frsize,
                                         "available_inodes": space.f_favail}
    return report


def run(inventory, inventory_only):
    mounts = filesystem_mounts()
    routes, reservations = load_inventory(inventory, mounts)
    deep = routes.pop("deep")
    conflicts = set(network_conflicts(deep, reservations.items()))
    unknown = []
    for other in routes.values():
        conflicts.update(directory_conflicts(deep, other))
        conflicts.update(resource_conflicts(deep, other))
        conflicts.update(network_conflicts(deep, ((other.route_id, peer) for peer in other.networks)))
    for index, port in enumerate(deep.ports):
        for peer in deep.ports[index + 1:]:
            if port.overlaps(peer):
                conflicts.add(f"deep {port} conflicts with deep {peer}")
    report = {"route_id": "deep", "scope": "inventory" if inventory_only else "host"}
    if not inventory_only:
        report["host"] = inspect_host(deep, mounts, conflicts, unknown)
    report.update(status="conflict" if conflicts else "unknown" if unknown else "clear",
                  conflicts=sorted(conflicts), unknown=unknown)
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inventory", required=True, type=Path)
    parser.add_argument("--inventory-only", action="store_true", help="check declared reservations without live probes")
    args = parser.parse_args()
    try:
        report = run(json.loads(args.inventory.read_text()), args.inventory_only)
    except (OSError, ValueError, TypeError, KeyError, AttributeError, RuntimeError) as error:
        report = {"route_id": "deep", "scope": "inventory" if args.inventory_only else "host",
                  "status": "unknown", "conflicts": [], "unknown": [str(error)]}
    print(json.dumps(report, indent=2))
    return {"clear": 0, "conflict": 1, "unknown": 2}[report["status"]]


if __name__ == "__main__":
    raise SystemExit(main())
