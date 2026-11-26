#!/usr/bin/env python3
"""Run offline Docker Compose or K3s DNS workloads in a managed System guest."""

import argparse
from contextlib import ExitStack
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import sys
import tempfile
import time

from managed_system_vsock import Platform, digest, wait_for


def copy_fixture(args, root):
    original = args.system_image
    disk = root / "workload.ext4"
    mount = root / "rootfs"
    mount.mkdir()
    subprocess.run(["cp", "--reflink=auto", "--sparse=always", original, disk], check=True)
    with ExitStack() as cleanup:
        subprocess.run(["mount", "-o", "loop,nosuid,nodev,noexec", disk, mount], check=True)
        cleanup.callback(subprocess.run, ["umount", mount], check=True)
        for source, target in (("agent", "usr/local/bin/smolvm-agent"),
                               ("identity-helper", "sbin/smolvm-identity-helper")):
            subprocess.run(["install", "-m", "0755", args.bundle / "guest" / source, mount / target], check=True)
        if args.case == "k3s":
            previous = mount / "var/lib/rancher/k3s"
            images = mount / "var/lib/rancher/a17/agent/images"
            images.mkdir(parents=True)
            archive = "k3s-airgap-images-amd64.tar.zst"
            (previous / "agent/images" / archive).rename(images / archive)
            # The archived input was booted before. Preserve its offline images
            # and use a new node, datastore, container cache and DNS deployment.
            shutil.rmtree(previous)
            subprocess.run(["fstrim", mount], check=False)
    disk.chmod(0o400)
    print(f"{args.case} offline System image {digest(original)} from {original.name}", flush=True)
    args.system_image = disk


def docker(guest):
    print("Docker", guest("/usr/bin/docker", "version", "--format", "{{.Server.Version}}").decode().strip(), flush=True)
    print("Compose", guest("/usr/bin/docker-compose", "version", "--short").decode().strip(), flush=True)
    path = "/opt/deep-docker-smoke"
    tag = "a17-workload:local"
    built = guest("/usr/bin/docker", "build", "--no-cache", "-t", tag, path)
    assert b"Successfully tagged" in built, built
    marker = "a17-docker-" + secrets.token_hex(8)
    spec = json.dumps({"version": "3.3", "services": {"probe": {
        "image": tag, "network_mode": "none", "command": ["/bin/busybox", "echo", marker],
    }}})
    guest("/bin/sh", "-ec", "cat > /tmp/a17-compose.json", stdin=spec)
    result = guest("/usr/bin/docker-compose", "-p", "a17", "-f", "/tmp/a17-compose.json", "up",
                   "--abort-on-container-exit", "--exit-code-from", "probe")
    assert marker.encode() in result, result
    container = guest("/usr/bin/docker", "inspect", "--format", "{{.Id}} {{.State.ExitCode}}", "a17_probe_1")
    identifier, code = container.split()
    assert identifier and code == b"0", container
    print(f"Compose built a local image and its container {identifier.decode()[:12]} printed {marker} with exit 0.", flush=True)

    # Reconnect the ordinary System management service while a real Docker
    # workload is still owned by the distribution's systemd and containerd.
    guest("/usr/bin/docker", "run", "--detach", "--name", "a17-system-reconnect", "--network", "none", tag,
          "/bin/busybox", "sh", "-ec", f"echo {marker}; exec /bin/busybox sleep 90")

    def agent_pid():
        return int(guest("/bin/systemctl", "show", "--property=MainPID", "--value", "smolvm-agent.service"))

    def workload_pid():
        return int(guest("/usr/bin/docker", "inspect", "--format", "{{.State.Pid}}", "a17-system-reconnect"))

    before = agent_pid()
    child = workload_pid()
    assert before > 1 and child > 1
    stamp = guest("/bin/cat", f"/proc/{child}/stat").rsplit(b") ", 1)[1].split()[19]
    guest("/usr/bin/systemd-run", "--unit=a17-restart-agent", "--on-active=1s", "--timer-property=AccuracySec=50ms",
          "/bin/systemctl", "restart", "smolvm-agent.service")
    after = wait_for(lambda: pid if (pid := agent_pid()) > 1 and pid != before else None,
                     timeout=20, retry=(OSError, RuntimeError))
    assert workload_pid() == child
    assert guest("/bin/cat", f"/proc/{child}/stat").rsplit(b") ", 1)[1].split()[19] == stamp
    assert guest("/usr/bin/docker", "logs", "a17-system-reconnect").strip() == marker.encode()
    print(f"System Agent restarted from {before} to {after}. Docker kept PID {child}, its start time and marker.", flush=True)


def k3s(guest):
    binary = "/usr/local/bin/k3s"
    app_ip = "10.73.0.42"
    marker = "a17-k3s-" + secrets.token_hex(8)
    image = "docker.io/rancher/mirrored-library-busybox:1.37.0"
    print(guest(binary, "--version").decode().splitlines()[0], flush=True)
    guest("/bin/sh", "-ec", '''
        ip link add a17-net type dummy
        ip addr add 192.0.2.20/24 dev a17-net
        ip link set a17-net up
        ip route replace default via 192.0.2.1 dev a17-net
        printf '%s\n' 'nameserver 192.0.2.1' > /run/a17-resolv.conf
        test -s /var/lib/rancher/a17/agent/images/k3s-airgap-images-amd64.tar.zst
    ''')
    guest("/usr/bin/systemd-run", "--unit=a17-k3s", "--property=Delegate=yes", "--property=TasksMax=infinity",
          binary, "server", "--data-dir=/var/lib/rancher/a17", "--write-kubeconfig=/run/a17-kubeconfig",
          "--node-ip=192.0.2.20", "--advertise-address=192.0.2.20", "--flannel-iface=a17-net",
          "--cluster-cidr=10.72.0.0/16", "--service-cidr=10.73.0.0/16", "--cluster-dns=10.73.0.10",
          "--resolv-conf=/run/a17-resolv.conf",
          "--disable=traefik,metrics-server,servicelb,local-storage")

    def kube(*command, stdin=None):
        return guest(binary, "kubectl", "--kubeconfig=/run/a17-kubeconfig", "--request-timeout=5s", *command, stdin=stdin)

    def apply(*resources):
        return kube("apply", "-f", "-", stdin=json.dumps({"apiVersion": "v1", "kind": "List", "items": resources}))

    def workload(name, command, **settings):
        return {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": name, "namespace": "a17", "labels": {"app": name}},
                "spec": {"containers": [{"name": name, "image": image, "imagePullPolicy": "Never",
                                        "command": ["/bin/busybox", "sh", "-ec", command]}], **settings}}

    try:
        wait_for(lambda: kube("wait", "--for=condition=Ready", "node", "--all", "--timeout=4s"), timeout=130,
                 retry=(RuntimeError, OSError))
        wait_for(lambda: kube("-n", "kube-system", "rollout", "status", "deployment/coredns", "--timeout=4s"),
                 timeout=100, retry=(RuntimeError, OSError))
        print(kube("get", "nodes", "-o", "wide").decode(), end="", flush=True)
        print(kube("-n", "kube-system", "get", "pods", "-l", "k8s-app=kube-dns", "-o", "wide").decode(), end="", flush=True)
        apply({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "a17"}})
        server = workload("echo", f'mkdir /tmp/web; printf %s {marker} > /tmp/web/index.html; '
                                  'exec /bin/busybox httpd -f -p 8080 -h /tmp/web')
        server["spec"]["containers"][0]["readinessProbe"] = {"httpGet": {"path": "/", "port": 8080}, "periodSeconds": 1}
        apply(server,
              {"apiVersion": "v1", "kind": "Service", "metadata": {"namespace": "a17", "name": "echo"},
               "spec": {"clusterIP": app_ip, "selector": {"app": "echo"}, "ports": [{"port": 8080}]}})
        wait_for(lambda: kube("-n", "a17", "wait", "--for=condition=Ready", "pod/echo", "--timeout=4s"),
                 timeout=40, retry=(RuntimeError, OSError))
        apply(workload("resolver", '''
            /bin/busybox nslookup -type=A kubernetes.default.svc.cluster.local
            /bin/busybox nslookup -type=A echo.a17.svc.cluster.local
            /bin/busybox wget -T 5 -qO- http://echo.a17.svc.cluster.local:8080/
        ''', restartPolicy="Never"))
        wait_for(lambda: kube("-n", "a17", "wait", "--for=jsonpath={.status.phase}=Succeeded", "pod/resolver", "--timeout=4s"),
                 timeout=40, retry=(RuntimeError, OSError))
        result = kube("-n", "a17", "logs", "resolver").decode()
        assert "10.73.0.1" in result and app_ip in result and result.strip().endswith(marker), result
        print(result, flush=True)
        print("A real Pod resolved the Kubernetes and workload Services via CoreDNS, then read the workload marker over HTTP.", flush=True)
    except BaseException:
        for command in (("journalctl", "-u", "a17-k3s", "-n", "35", "--no-pager"),
                        (binary, "kubectl", "--kubeconfig=/run/a17-kubeconfig", "--request-timeout=4s", "get", "pods", "-A", "-o", "wide"),
                        (binary, "kubectl", "--kubeconfig=/run/a17-kubeconfig", "--request-timeout=4s", "-n", "a17", "logs", "resolver")):
            try:
                print(guest(*command).decode(errors="replace"), file=sys.stderr)
            except (OSError, RuntimeError) as error:
                print(error, file=sys.stderr)
        raise


def test(args, root, stack):
    copy_fixture(args, root)
    # Real block storage can reclaim its page cache within the VM memory
    # budget. tmpfs would charge every QCOW2 write as unreclaimable shmem.
    catalog = root / "catalog"
    catalog.mkdir()
    storage = root / "catalog.ext4"
    with storage.open("xb") as image:
        image.truncate(6 << 30)
    subprocess.run(["mkfs.ext4", "-q", "-F", "-m", "0", storage], check=True)
    subprocess.run(["mount", "-o", "loop,nosuid,nodev", storage, catalog], check=True)
    stack.callback(subprocess.run, ["umount", "--lazy", catalog], check=True)
    catalog.chmod(0o700)
    platform = Platform(args, root, stack, state_tmpfs=True, use_installation=True)
    platform.start()
    args.system_image.unlink()
    spec = {
        "name": "System container fixture", "template_id": "t_system", "boot_mode": "system",
        "runtime_instance_id": platform.serve_id, "start": True,
        "resources": {"cpu_count": 2, "memory_mib": 2304 if args.case == "k3s" else 768},
        "lifecycle_policy": {"auto_hibernate": False, "auto_standby": False, "keep_running": True},
    }
    deadline = time.monotonic() + 120
    attempt = 0
    while True:
        machine = platform.change("machines", f"create-a17-system-{attempt}", spec)
        try:
            platform.complete(machine)
            break
        except RuntimeError as error:
            row = error.args[0]
            proof = (row.get("error") or {}) if isinstance(row, dict) else {}
            retry = (isinstance(row, dict) and row.get("effect") == "none" and proof.get("code") == "RESOURCE_LIMIT"
                     and "memory admission is throttled" in proof.get("message", ""))
            if not retry or time.monotonic() >= deadline:
                raise
            print("The Worker deferred memory admission. Retrying the unpublished fixture launch.", flush=True)
            attempt += 1
            time.sleep(10)
    _, ref = platform.context(machine["machine_id"])

    def guest(*command, stdin=None):
        return platform.guest(ref, list(command), stdin)

    wait_for(lambda: guest("/bin/cat", "/proc/1/comm") == b"systemd\n", retry=(OSError, RuntimeError))
    assert guest("/bin/findmnt", "-n", "-o", "SOURCE,FSTYPE", "/").strip() == b"/dev/vda ext4"
    assert guest("/usr/bin/sha256sum", "/usr/local/bin/smolvm-agent").decode().split()[0] == digest(args.bundle / "guest/agent").removeprefix("sha256:")
    guest("/bin/systemctl", "is-active", "smolvm-agent.service")
    print(guest("/bin/uname", "-r").decode().strip(), "systemd PID 1, writable /dev/vda ext4", flush=True)
    if args.case == "docker":
        guest("/bin/systemctl", "start", "docker.service")
        docker(guest)
    else:
        k3s(guest)
    print(f"{args.case} System workload verified on {platform.bundle_id}", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ("--bundle", "--platformd", "--system-image"):
        parser.add_argument(option, required=True, type=lambda value: Path(value).resolve(strict=True))
    parser.add_argument("--case", required=True, choices=("docker", "k3s"))
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    args = parser.parse_args()
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("requires root, KVM and a running systemd manager")
    if os.environ.get("SYSTEM_CONTAINERS_TEST_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, __file__, *sys.argv[1:]], {**os.environ, "SYSTEM_CONTAINERS_TEST_NAMESPACE": "1"})
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="a17s-") as directory, ExitStack() as stack:
        root = Path(directory)
        subprocess.run(["mount", "-t", "tmpfs", "-o", "size=6G,mode=0755,nosuid,nodev",
                        "tmpfs", root], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", root], check=True)
        try:
            test(args, root, stack)
        except BaseException:
            for name in ("platform.log", "worker.log", "serve.log"):
                log = root / name
                if log.exists():
                    print(name, log.read_text(errors="replace")[-3000:], file=sys.stderr)
            raise


if __name__ == "__main__":
    main()
