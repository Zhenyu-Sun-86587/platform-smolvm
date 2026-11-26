#!/usr/bin/env python3
"""Verify an OCI workload and VMM survive loss of the Guest management Agent."""

import argparse
import base64
from contextlib import ExitStack
import json
import os
from pathlib import Path
import runpy
import secrets
import select
import shutil
import signal
import socket
import struct
import subprocess
import sys
import tempfile

from lifecycle_wake_collector_kvm import readonly_bind
from managed_system_vsock import digest, wait_for


def request(path, body, timeout=4):
    payload = json.dumps(body).encode()
    with socket.socket(socket.AF_UNIX) as connection:
        connection.settimeout(timeout)
        connection.connect(str(path))
        connection.sendall(struct.pack(">I", len(payload)) + payload)
        with connection.makefile("rb") as stream:
            header = stream.read(4)
            if len(header) != 4:
                raise ConnectionError("Guest Agent disconnected")
            length = struct.unpack(">I", header)[0]
            assert length <= 1 << 20, length
            return json.loads(stream.read(length))


def guest(path, command, *, background=False):
    response = request(path, {"method": "vm_exec", "command": command,
                              "background": background, "timeout_ms": 4000})
    assert response["status"] == "completed" and response["exit_code"] == 0, response
    return base64.b64decode(response["stdout"]).decode().strip()


def process_identity(stat):
    pid, _, tail = stat.rpartition(") ")
    return int(pid.split()[0]), int(tail.split()[19])


def guest_identity(path, marker):
    agent, supervisor, init, boot, workload = guest(path, ["/bin/sh", "-ec", r'''
        marker=$1
        agent=$(cat "/proc/$PPID/stat")
        printf '%s\n' "$agent"
        set -- ${agent##*) }
        cat "/proc/$2/stat"
        cat /proc/1/comm
        cat /proc/sys/kernel/random/boot_id
        for process in /proc/[0-9]*; do
            command=$(tr '\000' ' ' 2>/dev/null < "$process/cmdline") || continue
            if [ "$command" = "/probe run $marker " ]; then cat "$process/stat"; fi
        done
    ''', "identify", marker]).splitlines()
    return process_identity(agent), init, boot, process_identity(workload), process_identity(supervisor)


def workload_registry(path):
    filesystem, checksum, _ = guest(path, ["/bin/sh", "-ec", '''
        set -- /tmp/smolvm-workloads-*/registry.json
        test "$#" = 1
        stat -f -c %T "$1"
        sha256sum "$1"
    ''']).split()
    assert filesystem == "tmpfs", filesystem
    return checksum


def restore_tmpfs_path(root):
    instance = (root / "state/.smolvm-deep-state-v1").read_text().splitlines()[-1]
    assert len(instance) == 32 and all(char in "0123456789abcdef" for char in instance), instance
    return Path("/dev/shm") / ("smolvmdeep-restore-" + instance)


def prepare_restore_tmpfs(root, stack):
    local_shm = root / "shm"
    local_shm.mkdir(mode=0o700)
    subprocess.run(["mount", "--bind", local_shm, "/dev/shm"], check=True)
    stack.callback(subprocess.run, ["umount", "--lazy", "/dev/shm"], check=True)
    restore = restore_tmpfs_path(root)
    restore.mkdir(mode=0o700)
    subprocess.run(["mount", "-t", "tmpfs", "-o", "size=1G,mode=0700,nosuid,nodev",
                    "tmpfs", restore], check=True)
    stack.callback(subprocess.run, ["umount", "--lazy", restore], check=True)


def prepare_restore_disk(root, stack):
    backing = root / "staging.ext4"
    with backing.open("wb") as stream:
        stream.truncate(1536 << 20)
    subprocess.run(["mkfs.ext4", "-q", "-m", "0", backing], check=True)
    disk = root / "disk"
    disk.mkdir()
    subprocess.run(["mount", "-o", "loop,nosuid,nodev", backing, disk], check=True)
    stack.callback(subprocess.run, ["umount", "--lazy", disk], check=True)
    return disk


def restore_registry(machine, root, source, marker, args, stack):
    kind = args.end
    checksum = workload_registry(source)
    original = guest_identity(source, marker)
    original_vmm = (source.parent / "agent.pid").read_text()
    name = "a17-oci" if kind == "resume" else "a17-child"
    if kind != "resume":
        stack.callback(machine, "stop", name=name, check=False)
    if kind == "branch":
        machine("branch", "--from", "a17-oci", "--freeze-source", "--no-wait-ready", name=name)
    elif kind == "resume":
        machine("pause")
        machine("resume", trace_restore=True)
    else:
        point = root / "oci.smolcheckpoint"
        machine("checkpoint", "--output", point, "--staging-dir", root)
        machine("create", "--from", point, name=name)
        if args.without_restore_tmpfs:
            restored = Path(machine("data-dir", name=name))
            retained = restored / ".restore-input/memory.bin"
            assert retained.is_file() and retained.stat().st_dev == root.stat().st_dev, retained
            print("Portable create retained verified RAM on the state ext4 filesystem.", flush=True)
        machine("start", name=name)
    target = Path(machine("data-dir", name=name)) / "agent.sock"
    assert (target.parent / "agent.pid").read_text() != original_vmm
    before = wait_for(lambda: guest_identity(target, marker), timeout=25, retry=(OSError, ValueError, AssertionError))
    assert before[3] == original[3] and workload_registry(target) == checksum, (original, before)
    guest(target, ["/bin/sh", "-ec", 'sleep 0.3; kill -KILL "$1"', "restart", str(before[0][0])], background=True)
    after = wait_for(lambda: row if (row := guest_identity(target, marker))[0] != before[0] else None,
                     timeout=20, retry=(OSError, ValueError))
    assert after[1:] == before[1:] and workload_registry(target) == checksum, (before, after)
    record = machine("exec", "/probe", "observe", name=name).split()
    assert record[0] == marker and int(record[2]) > 1, record
    print(f"The {kind} target and its replacement Agent read the same Guest tmpfs registry {checksum} and crun PID {after[3]}.", flush=True)


def test(args, root, stack):
    for name in ("bundle", "guest", "home", "runtime", "state", "var-lib", "witness"):
        (root / name).mkdir()
    (root / "witness").chmod(0o777)
    bundle = root / "bundle"
    readonly_bind(stack, args.bundle, bundle)
    if args.smolvm_bin is not None:
        readonly_bind(stack, args.smolvm_bin, bundle / "smolvm-bin")
        print("Testing a private development smolvm binary", digest(bundle / "smolvm-bin"), flush=True)
    subprocess.run(["mount", "--bind", root / "var-lib", "/var/lib"], check=True)
    stack.callback(subprocess.run, ["umount", "/var/lib"], check=True)
    subprocess.run(["tar", "--no-overwrite-dir", "-xzf", str(bundle / "guest/oci-rootfs.tar.gz"),
                    "-C", root / "guest"], check=True)
    if args.agent is not None:
        target = root / "guest/usr/local/bin/smolvm-agent"
        shutil.copyfile(args.agent, target)
        target.chmod(0o755)
        print("Testing a private development OCI Agent", digest(target), flush=True)

    probe = args.probe
    if probe is None:
        probe = root / "probe"
        source = Path(__file__).resolve().parents[1] / "fixtures/oci-agent-workload.c"
        subprocess.run([args.cc, "-static", "-Os", "-s", "-o", probe, source], check=True)
    packaging = Path(__file__).resolve().parents[2] / "packaging/deep"
    sys.path.insert(0, str(packaging))
    image = root / "image.tar"
    runpy.run_path(packaging / "smoke-candidate.py")["write_image"](probe, image)
    env = {"PATH": os.environ["PATH"], "LC_ALL": "C", "HOME": str(root / "home"),
           "SMOLVM_AGENT_ROOTFS": str(root / "guest"), "SMOLVM_LIB_DIR": str(bundle / "lib"),
           "XDG_RUNTIME_DIR": str(root / "runtime"), "TMPDIR": str(root)}

    def machine(action, *flags, name="a17-oci", check=True, trace_restore=False):
        command_env = ({**env, "SMOLVM_LOG_FORMAT": "json", "RUST_LOG": "smolvm::portable_checkpoint=info"}
                       if trace_restore else env)
        result = subprocess.run([bundle / "smolvm", "--state-dir", root / "state", "machine", action,
                                 "--name", name, *flags], env=command_env, text=True, capture_output=True, timeout=60)
        if check and result.returncode != 0:
            raise RuntimeError((action, result.returncode, result.stdout, result.stderr))
        if trace_restore:
            fields = [json.loads(line)["fields"] for line in result.stderr.splitlines() if line.startswith("{")]
            staging = str(restore_tmpfs_path(root))
            assert [row["staging"] for row in fields if row.get("phase") == "extract"] == [staging], fields
            assert any(row.get("asset") == "checkpoint/memory.bin" and row.get("method") == "readonly_link"
                       for row in fields), fields
            print(f"Resume extracted and retained RAM on the configured tmpfs {staging}.", flush=True)
        return result.stdout.strip()

    marker = secrets.token_hex(12)
    policy = ("--stop-on-exit",) if args.end == "workload" else ()
    recovers = args.end in ("branch", "snapshot", "resume")
    volumes = () if recovers else ("--volume", str(root / "witness") + ":/witness")
    machine("create", "--cpus", "1", "--mem", "384", "--storage", "1", "--overlay", "1",
            *volumes, "--image", image, "--stop-signal", "SIGUSR1",
            *policy, "--", "/probe", "run", marker)
    if args.end in ("snapshot", "resume"):
        if args.without_restore_tmpfs:
            assert not restore_tmpfs_path(root).exists()
        else:
            prepare_restore_tmpfs(root, stack)
    stack.callback(machine, "stop", check=False)
    machine("start", *(("--branchable",) if recovers else ()))
    state = Path(machine("data-dir"))
    agent = state / "agent.sock"
    measured_agent = digest(args.agent or bundle / "guest/agent").removeprefix("sha256:")
    assert guest(agent, ["/bin/sh", "-ec", 'sha256sum "/proc/$PPID/exe"']).split()[0] == measured_agent
    print("Guest Agent version", guest(agent, ["/usr/local/bin/smolvm-agent", "--version"]), flush=True)
    if recovers:
        restore_registry(machine, root, agent, marker, args, stack)
        return
    started = root / "witness/started"
    progress = root / "witness/progress"
    before = wait_for(lambda: progress.read_text().split(), timeout=20)
    assert started.read_text().split() == before[:2] and before[0] == marker
    initial = guest_identity(agent, marker)
    registry = None if args.expect_legacy_exit else workload_registry(agent)
    vmm = tuple(int(n) for n in (state / "agent.pid").read_text().split())
    assert process_identity(Path(f"/proc/{vmm[0]}/stat").read_text()) == vmm
    print(f"Original VMM {vmm}, Guest PID1 {initial[1]}, Agent {initial[0]} with parent {initial[4]}, crun workload {initial[3]}, marker {marker}.", flush=True)
    with os.fdopen(os.pidfd_open(vmm[0]), "rb", buffering=0) as live:
        guest(agent, ["/bin/sh", "-ec", '''
            sleep 0.3; kill -STOP "$1"; sleep 2; kill "$2" "$1"
            if [ "$2" = -TERM ]; then kill -CONT "$1"; fi
        ''', "restart", str(initial[0][0]), "-" + args.agent_signal], background=True)

        def unavailable():
            try:
                request(agent, {"method": "ping"}, timeout=0.25)
            except OSError:
                return True

        wait_for(unavailable, timeout=5)
        advancing = wait_for(lambda: row if int((row := progress.read_text().split())[2]) > int(before[2]) else None,
                             timeout=5)
        assert advancing[:2] == before[:2]
        print(f"The Agent stopped answering while the original crun workload advanced before SIG{args.agent_signal}.", flush=True)
        if args.expect_legacy_exit:
            assert select.select([live], [], [], 15)[0], "the historical VMM unexpectedly survived Agent exit"
            assert not (root / "witness/duplicate").exists()
            print("Historical OCI init shut down the original VMM when its ordinary Agent exited.", flush=True)
            return

        current = wait_for(lambda: info if (info := guest_identity(agent, marker))[0] != initial[0] else None,
                           timeout=20, retry=(OSError, ValueError))
        assert not select.select([live], [], [], 0)[0]
        assert process_identity(Path(f"/proc/{vmm[0]}/stat").read_text()) == vmm
        assert current[1:] == initial[1:] and current[0][0] != initial[0][0], (initial, current)
        assert workload_registry(agent) == registry
        recovered = machine("exec", "/probe", "observe").split()
        assert recovered[:2] == before[:2] and int(recovered[2]) > int(advancing[2]), (advancing, recovered)
        assert started.read_text().split() == before[:2] and not (root / "witness/duplicate").exists()
        assert current[4][0] > 1 and guest(agent, ["/bin/cat", f"/proc/{current[4][0]}/comm"]) == "smolvm-oci-init"

        def live_logs():
            for line in reversed(machine("logs", "--tail", "64").splitlines()):
                fields = line.split()
                if fields[:2] == ["A17_LOG", marker] and int(fields[2]) > int(advancing[2]):
                    return fields[2]

        logged = wait_for(live_logs, timeout=8)
        print(f"Agent recovered as {current[0]} and reconnected the original crun PID {current[3]} with the same marker and advancing count.", flush=True)
        print(f"Workload stdout crossed the Agent interruption through count {logged}.", flush=True)
        if args.end == "workload":
            (root / "witness/finish").touch()
            if not select.select([live], [], [], 12)[0]:
                try:
                    print(guest(agent, ["/bin/sh", "-ec", '''
                        for task in /proc/"$1"/task/*; do
                            printf '%s ' "$(cat "$task/comm")"
                            cat "$task/wchan" "$task/syscall"
                        done
                    ''', "diagnose", str(current[0][0])]), file=sys.stderr)
                except (OSError, AssertionError) as error:
                    print("Guest Agent after workload exit", error, file=sys.stderr)
                raise AssertionError("stop-on-exit left the exact VMM running")
            assert (root / "witness/exited").read_text().split() == [*before[:2], "0"]
            print("The original workload exited on its own and stop-on-exit powered off the exact VMM.", flush=True)
        elif args.end == "supervisor":
            guest(agent, ["/bin/sh", "-ec", 'sleep 0.3; kill -TERM "$1"', "stop", str(current[4][0])], background=True)
            assert select.select([live], [], [], 12)[0], "supervisor SIGTERM left its VMM running"
            print("SIGTERM addressed to the OCI supervisor shut down the exact VMM.", flush=True)
        else:
            machine("stop")
            assert select.select([live], [], [], 12)[0], "explicit machine stop left the original VMM alive"
            assert (root / "witness/exited").read_text().split() == [*before[:2], str(signal.SIGUSR1)]
            print("Explicit machine stop sent the registered SIGUSR1 to the original crun PID and terminated the exact VMM.", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bundle", required=True, type=lambda value: Path(value).resolve(strict=True))
    parser.add_argument("--probe", type=lambda value: Path(value).resolve(strict=True))
    parser.add_argument("--agent", type=lambda value: Path(value).resolve(strict=True), help="private development OCI Agent")
    parser.add_argument("--smolvm-bin", type=lambda value: Path(value).resolve(strict=True), help="private development CLI and VMM")
    parser.add_argument("--cc", default="cc")
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    parser.add_argument("--expect-legacy-exit", action="store_true")
    parser.add_argument("--agent-signal", choices=("KILL", "TERM"), default="KILL")
    parser.add_argument("--end", choices=("manual", "workload", "supervisor", "branch", "snapshot", "resume"), default="manual")
    parser.add_argument("--without-restore-tmpfs", action="store_true", help="stage a portable restore on a private ext4 filesystem")
    args = parser.parse_args()
    if args.agent is not None and args.expect_legacy_exit:
        parser.error("historical control uses the original OCI Agent")
    if args.without_restore_tmpfs and args.end != "snapshot":
        parser.error("--without-restore-tmpfs requires --end snapshot")
    if os.geteuid() != 0 or not Path("/dev/kvm").exists():
        parser.error("requires root and KVM")
    if os.environ.get("OCI_AGENT_TEST_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, __file__, *sys.argv[1:]], {**os.environ, "OCI_AGENT_TEST_NAMESPACE": "1"})
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="a17o-") as directory, ExitStack() as stack:
        root = Path(directory)
        capacity = "1500M" if args.end in ("snapshot", "resume") else "512M"
        subprocess.run(["mount", "-t", "tmpfs", "-o", f"size={capacity},mode=0755,nosuid,nodev",
                        "tmpfs", root], check=True)
        stack.callback(subprocess.run, ["umount", "--lazy", root], check=True)
        if args.without_restore_tmpfs:
            root = prepare_restore_disk(root, stack)
        try:
            test(args, root, stack)
        except BaseException:
            for log in (root / "state").rglob("agent-console.log"):
                print(log.name, log.read_text(errors="replace")[-3500:], file=sys.stderr)
            raise
    manifest = json.loads((args.bundle / "runtime-bundle.json").read_bytes())
    print("OCI Agent experiment used", manifest["bundle_digest"], flush=True)


if __name__ == "__main__":
    main()
