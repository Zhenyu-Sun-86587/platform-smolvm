#!/usr/bin/env python3
"""Exercise TCP, DNS hairpin, IPv4 MTU and peering on owned KVM networks."""

import argparse
from contextlib import ExitStack
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import tempfile

from authorized_services_kvm import Platform, identity, witness, worker_get
from managed_system_vsock import digest, wait_for, worker


PROGRAM = "/run/a13-network.pl"
TCP = 18131
UDP = 18132
CIDR = "10.44.62.0/24"
PROBE = r'''
use strict;
use warnings;
use IO::Socket::INET;
use Socket qw(IPPROTO_IP inet_ntoa sockaddr_in);
use Errno qw(EMSGSIZE);

my ($mode, $port, $destination, $id, $size) = @ARGV;
my $protocol = $mode =~ /udp$/ ? 'udp' : 'tcp';

sub reply {
    my ($label, $address, $data) = @_;
    my ($id) = $data =~ /^([a-z0-9-]{1,64})(?:\n|\x00x*)$/;
    die 'unexpected fixture message' unless defined $id;
    return "$label $address $id " . length($data) . "\n";
}

if ($mode =~ /^send-/) {
    local $SIG{ALRM} = sub { die "network fixture reply timed out\n"; };
    alarm 10;
    my $client = IO::Socket::INET->new(Proto => $protocol, PeerAddr => $destination, PeerPort => $port) or die $!;
    if ($protocol eq 'udp') {
        # Linux IP_MTU_DISCOVER = 10, IP_PMTUDISC_DO = 2.
        setsockopt($client, IPPROTO_IP, 10, pack('i', 2)) or die $!;
        my $payload = $id . "\x00" . ('x' x ($size - length($id) - 1));
        unless (defined $client->send($payload)) {
            die $! unless $! == EMSGSIZE;
            print "EMSGSIZE\n";
            exit;
        }
        $client->recv(my $answer, 256) or die $!;
        print $answer;
    } else {
        print {$client} "$id\n";
        print scalar(<$client>) // '';
    }
    exit;
}

my $label = $destination;
my %local = (Proto => $protocol, LocalAddr => '0.0.0.0', LocalPort => $port, ReuseAddr => 1);
if ($protocol eq 'udp') {
    my $server = IO::Socket::INET->new(%local) or die $!;
    while (my $peer = $server->recv(my $data, 65536)) {
        my (undef, $address) = sockaddr_in($peer);
        $server->send(reply($label, inet_ntoa($address), $data), 0, $peer);
    }
    exit;
}
my $server = IO::Socket::INET->new(%local, Listen => 16) or die $!;
while (my $client = $server->accept()) {
    my $data = <$client> // '';
    print {$client} reply($label, $client->peerhost(), $data);
    close $client;
}
'''


def launch(platform, label, network, mode):
    created = platform.complete(platform.change("machines", "a13-create-" + label, {
        "name": "Owned " + label, "boot_mode": "system", "template_id": "t_system",
        "runtime_instance_id": platform.serve_id, "start": True,
        "resources": {"cpu_count": 1, "memory_mib": 512},
        "network": {"network_id": network["network_id"], "mode": mode},
    }))
    machine = created["machine_id"]
    _, execution = platform.context(machine)
    wait_for(lambda: platform.guest(execution, ["cat", "/proc/1/comm"]) == b"systemd\n",
             retry=(RuntimeError, OSError))
    endpoint = wait_for(lambda: (record := platform.app(f"machines/{machine}/network"))["state"] == "published" and record)
    assert endpoint["mode"] == mode and endpoint["network_id"] == network["network_id"], endpoint
    observed = worker_get(platform, "QueryExecutionNetworkAttachment", execution)
    lease = observed["lease"]
    assert lease["attachment"]["route_domain_id"] == network["route_domain_id"], observed
    addresses = json.loads(platform.guest(execution, ["ip", "-4", "-j", "address", "show", "scope", "global"]))
    link, = [row for row in addresses if any(addr["local"] == endpoint["guest_ipv4"] for addr in row["addr_info"])]
    routes = json.loads(platform.guest(execution, ["ip", "-4", "-j", "route", "show", "default"]))
    default, = [row for row in routes if row["dev"] == link["ifname"]]

    platform.guest(execution, ["/bin/sh", "-ec", 'cat > "$1"', "owned", PROGRAM], stdin=PROBE)
    for protocol, port in (("tcp", TCP), ("udp", UDP)):
        platform.guest(execution, ["systemd-run", "--unit=a13-" + protocol, "/usr/bin/perl", PROGRAM,
                                    protocol, str(port), label])
        wait_for(lambda: f":{port:04X}".encode() in platform.guest(execution, ["cat", f"/proc/net/{protocol}"]))
    return {"label": label, "machine": machine, "execution": execution, "endpoint": endpoint,
            "guest_gateway": default["gateway"], "mtu": link["mtu"], "route_domain": network["route_domain_id"]}


def dns(platform, source, destination):
    expected = destination["endpoint"]
    name = expected["dns_name"]
    lines = platform.guest(source["execution"], ["getent", "ahostsv4", name]).decode().splitlines()
    assert {line.split()[0] for line in lines} == {expected["private_ipv4"]}, (name, lines, expected)
    return name


def flow(platform, source, destination, address, *, transport="tcp", size=0, hairpin=False, oversize=False):
    ident = "a13-" + secrets.token_hex(9)
    port = TCP if transport == "tcp" else UDP
    out = platform.guest(source["execution"], ["/usr/bin/perl", PROGRAM, "send-" + transport,
                                             str(port), address, ident, str(size)]).decode().strip()
    if oversize:
        assert out == "EMSGSIZE", out
        return {"result": out, "payload_bytes": size}
    expected_peer = source["guest_gateway"] if hairpin else source["endpoint"]["private_ipv4"]
    label, peer, seen, length = out.split()
    assert (label, peer, seen) == (destination["label"], expected_peer, ident), (out, expected_peer, destination)
    assert int(length) == (len(ident) + 1 if transport == "tcp" else size), out
    return {"from": source["label"], "to": label, "target": address, "observed_source": peer,
            "request": seen, "payload_bytes": int(length)}


def exercise(platform, networks, case):
    first = launch(platform, "alpha", networks[0], "fixed_internal")
    second = launch(platform, "beta", networks[-1], "native" if case == "mixed" else "fixed_internal")
    endpoints = [machine["endpoint"] for machine in (first, second)]
    if case == "mixed":
        assert endpoints[1]["guest_ipv4"] == endpoints[1]["private_ipv4"], endpoints
        assert endpoints[0]["guest_ipv4"] != endpoints[1]["guest_ipv4"], endpoints
    else:
        assert endpoints[0]["guest_ipv4"] == endpoints[1]["guest_ipv4"], endpoints
    assert endpoints[0]["attachment_id"] != endpoints[1]["attachment_id"], endpoints
    hairpins = []
    for local in ((first,) if case == "mixed" else (first, second)):
        own_name = dns(platform, local, local)
        hairpins.append(flow(platform, local, local, own_name, hairpin=True))
        hairpins.append(flow(platform, local, local, local["endpoint"]["private_ipv4"], hairpin=True))

    directions = []
    if case != "overlap":
        assert endpoints[0]["private_ipv4"] != endpoints[1]["private_ipv4"], endpoints
        for source, target in ((first, second), (second, first)):
            address = dns(platform, source, target)
            tcp = flow(platform, source, target, address)
            mtu = source["mtu"]
            assert mtu == target["mtu"] == 1500, (source, target)
            udp = flow(platform, source, target, target["endpoint"]["private_ipv4"], transport="udp", size=mtu - 28)
            over = flow(platform, source, target, target["endpoint"]["private_ipv4"], transport="udp", size=mtu - 27, oversize=True)
            directions.append({"tcp": tcp, "mtu": mtu, "udp_df": udp, "udp_over": over})
    else:
        assert endpoints[0]["private_ipv4"] == endpoints[1]["private_ipv4"], endpoints
        assert first["route_domain"] != second["route_domain"]

    for local in (first, second):
        assert identity(platform.context(local["machine"])[1]) == identity(local["execution"])
    witness(platform, "a13-authorized-" + case, endpoints=endpoints, hairpins=hairpins, directions=directions,
            route_domains=[first["route_domain"], second["route_domain"]])


class PeeringPlatform(Platform):
    def spawn(self, command, log):
        if log == "worker.log":
            self.worker_command = command
            command = [*command, "--private-network-peering-config", self.policy]
        return super().spawn(command, log)

    @property
    def policy(self):
        return self.root / "private-peering.json"

    def set_peers(self, pairs):
        temporary = self.policy.with_suffix(".next")
        with open(temporary, "x", opener=lambda path, flags: os.open(path, flags, 0o600)) as file:
            json.dump({"version": 1, "allowed_pairs": pairs}, file)
        temporary.replace(self.policy)

    def peer_status(self, expected):
        status = worker(self.worker, "QueryPrivateNetworkPeering")
        assert status["configured"] and status["error"] is None, status
        return status if status["requested"] == status["active"] == expected else None


def peering_routes(platform, networks):
    routes = {}
    for pin in (platform.root / "catalog/network-namespaces").glob("router-*"):
        table = json.loads(subprocess.check_output(["nsenter", f"--net={pin}", "--", "ip", "-4", "-j", "route"]))
        local, = [route["dst"] for route in table if route.get("type") == "blackhole"]
        routes[local] = table
    assert set(routes) == {network["cidr"] for network in networks}, routes
    left, right = (network["cidr"] for network in networks)
    return [[route for route in routes[own] if route.get("dst") == peer] for own, peer in ((left, right), (right, left))]


def exercise_peering(platform, networks):
    first = launch(platform, "alpha", networks[0], "fixed_internal")
    second = launch(platform, "beta", networks[1], "native")
    default = wait_for(lambda: platform.peer_status([]))
    assert peering_routes(platform, networks) == [[], []]
    peers = [{"left": networks[0]["network_id"], "right": networks[1]["network_id"]}]
    platform.set_peers(peers)
    allowed = wait_for(lambda: platform.peer_status(peers))

    def connection_routes():
        routes = peering_routes(platform, networks)
        assert all(len(entries) == 1 and entries[0].get("dev", "").startswith("sp") for entries in routes), routes
        return routes

    def connections():
        return [flow(platform, source, target, target["endpoint"]["private_ipv4"])
                for source, target in ((first, second), (second, first))]

    active_routes = connection_routes()
    connected_before = connections()
    initial = worker(platform.worker, "Health")
    platform.broker.kill()
    platform.broker.wait(timeout=10)
    wait_for(lambda: peering_routes(platform, networks) == [[], []], timeout=10)
    platform.broker = platform.spawn(platform.worker_command, "worker.log")

    def recovered_worker():
        health = worker(platform.worker, "Health")
        return health if health["state"] == "ready" and health["broker_session_id"] != initial["broker_session_id"] else None

    recovered = wait_for(recovered_worker, timeout=65, retry=(OSError, RuntimeError))
    assert recovered["executable_sha256"] == initial["executable_sha256"] == digest(platform.bundle / "smolvm-resourced")
    wait_for(lambda: platform.peer_status(peers))
    recovered_routes = connection_routes()
    connected_after = connections()

    platform.set_peers([])
    revoked = wait_for(lambda: platform.peer_status([]))
    wait_for(lambda: peering_routes(platform, networks) == [[], []], timeout=10)
    for local in (first, second):
        ref = platform.context(local["machine"])[1]
        assert identity(ref) == identity(local["execution"])
        assert platform.guest(ref, ["cat", "/proc/1/comm"]) == b"systemd\n"
    witness(platform, "a13-authorized-peering", networks=networks, default=default, allowed=allowed, revoked=revoked,
            active_routes=active_routes, recovered_routes=recovered_routes, connected_before=connected_before,
            connected_after=connected_after, worker=initial["executable_sha256"],
            worker_sessions=[initial["broker_session_id"], recovered["broker_session_id"]],
            executions=[identity(local["execution"]) for local in (first, second)])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "platformd", "system-image"):
        parser.add_argument("--" + name, type=lambda value: Path(value).resolve(strict=True), required=True)
    parser.add_argument("--case", choices=("same-domain", "mixed", "overlap", "peering"), required=True)
    args = parser.parse_args()
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("requires root and KVM")
    if os.environ.get("A13_OWNED_NETWORK_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, "-B", __file__, *sys.argv[1:]],
                   {**os.environ, "A13_OWNED_NETWORK_NAMESPACE": "1", "PYTHONDONTWRITEBYTECODE": "1"})
    with tempfile.TemporaryDirectory(dir="/var/tmp", prefix="a13-owned-") as directory, ExitStack() as stack:
        root = Path(directory)
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=4G,mode=0755,nosuid,nodev", "tmpfs", root], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", root], check=True)
        unit = f"smolvmownednet{os.getpid()}_{secrets.token_hex(4)}.slice"
        subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                        "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                        unit, "fail", "1", "MemoryAccounting", "b", "true", "0"],
                       check=True, stdout=subprocess.DEVNULL)
        stack.callback(subprocess.run, ["systemctl", "stop", unit], check=True)
        os.environ["SMOLVM_WORKER_SLICE_NAME"] = unit
        networks = [dict(network_id="a13-alpha", route_domain_id="a13-alpha", cidr=CIDR)]
        if args.case == "overlap":
            networks.append(dict(network_id="a13-beta", route_domain_id="a13-beta", cidr=CIDR))
        if args.case == "peering":
            networks.append(dict(network_id="a13-beta", route_domain_id="a13-beta", cidr="10.44.63.0/24"))
        platform_type = PeeringPlatform if args.case == "peering" else Platform
        platform = platform_type(args, root, stack, networks, state_tmpfs=True, use_installation=True)
        try:
            if args.case == "peering":
                platform.set_peers([])
            platform.start()
            print(f"Platform {digest(args.platformd)}; System image {digest(args.system_image)}.", flush=True)
            if args.case == "peering":
                exercise_peering(platform, networks)
            else:
                exercise(platform, networks, args.case)
        except BaseException:
            for log in root.glob("*.log"):
                print(log.name, log.read_text(errors="replace")[-2000:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
