#!/usr/bin/env python3
"""Install and upgrade real releases on a disposable KVM host, then cut its power."""

import argparse
import json
import os
from pathlib import Path
import re
import select
import shutil
import subprocess
import tempfile
import time


def make_seed(args, scratch):
    # The guest receives one read-only block device containing just these inputs.
    source = scratch / "inputs"
    source.mkdir()
    for name, release in (("old", args.old_release), ("new", args.new_release)):
        target = source / name
        target.mkdir()
        manifest = release / "release-manifest.json"
        shutil.copy2(manifest, target)
        for member in json.loads(manifest.read_text())["files"]:
            path = Path(member["path"])
            if path.is_absolute() or ".." in path.parts:
                raise ValueError(f"invalid release member {path}")
            (target / path).parent.mkdir(exist_ok=True, parents=True)
            shutil.copy2(release / path, target / path)
    shutil.copy2(args.runtime_archive, source / "runtime.tar.gz")
    shutil.copy2(Path(__file__).with_name("release_host_guest.sh"), source / "guest.sh")
    modules = source / "modules"
    modules.mkdir()
    dependencies = subprocess.check_output(
        ["modprobe", "--set-version", args.kernel_release, "--show-depends", args.kvm_module], text=True)
    for line in dependencies.splitlines():
        method, _, name = line.strip().partition(" ")
        if method == "builtin":
            continue
        if method != "insmod":
            raise ValueError(f"unrecognized module dependency {line}")
        module = Path(name.strip())
        if module.suffix == ".zst":
            with (modules / module.stem).open("wb") as out:
                subprocess.run(["zstd", "-dc", module], stdout=out, check=True)
        else:
            shutil.copy2(module, modules)
    (source / "module-name").write_text(args.kvm_module + "\n")
    seed = scratch / "fixtures.squashfs"
    subprocess.run(["mksquashfs", source, seed, "-noappend", "-processors", "2", "-comp", "lz4",
                    "-no-progress", "-quiet"], check=True)
    shutil.rmtree(source)
    return seed


def boot(args, seed, disk, stage):
    cmd = [args.qemu, "-accel", "kvm", "-cpu", "host", "-smp", "2", "-m", "768",
           "-nodefaults", "-no-user-config", "-nographic", "-nic", "none", "-serial", "stdio",
           "-monitor", "none", "-no-reboot", "-kernel", args.kernel,
           "-append", ("root=/dev/vda rw console=ttyS0,115200 init=/bin/sh quiet panic=1 "
                       "sandbox.release_fixture=isolated systemd.mask=serial-getty@ttyS0.service "
                       "systemd.mask=smolvm-agent.service systemd.mask=sandbox-taskd.service "
                       "systemd.mask=sandbox-managed-sshd.service"),
           "-drive", f"file={disk},format=qcow2,if=virtio,cache=writethrough",
           "-drive", f"file={seed},format=raw,if=virtio,readonly=on"]
    if args.firmware_dir:
        cmd.extend(["-L", args.firmware_dir])
    if args.bios:
        cmd.extend(["-bios", args.bios])
    with subprocess.Popen(cmd, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.STDOUT) as process:
        started = False
        output = b""
        deadline = time.monotonic() + args.timeout
        try:
            while time.monotonic() < deadline and process.poll() is None:
                if not select.select([process.stdout], [], [], 1)[0]:
                    continue
                block = os.read(process.stdout.fileno(), 16384)
                if not block:
                    break
                print(block.decode(errors="replace"), end="", flush=True)
                output = (output + block)[-65536:]
                if not started and b"# " in output:
                    bootstrap = ("mount -t proc proc /proc && mount -t sysfs sysfs /sys && "
                                 "mkdir -p /mnt/release-fixture && "
                                 "mount -t squashfs -o ro /dev/vdb /mnt/release-fixture && "
                                 f"/bin/bash /mnt/release-fixture/guest.sh bootstrap {stage} && exec /sbin/init\n")
                    process.stdin.write(bootstrap.encode())
                    process.stdin.flush()
                    started = True
                if b"RELEASE_FIXTURE_FAILURE" in output:
                    raise RuntimeError(f"guest failed during {stage}")
                result = re.search(rb"RELEASE_FIXTURE_OK " + stage.encode() + rb" ([0-9a-f-]{36})\r?\n", output)
                if result:
                    return result[1].decode()
            raise RuntimeError(f"no {stage} receipt before timeout or QEMU exit ({process.poll()})")
        finally:
            # Only the QEMU process we created can be signalled here.
            if process.poll() is None:
                process.kill()
            process.wait()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("base-image", "kernel", "old-release", "new-release", "runtime-archive"):
        parser.add_argument("--" + name, type=lambda value: Path(value).resolve(strict=True), required=True)
    parser.add_argument("--kernel-release", default=os.uname().release)
    parser.add_argument("--kvm-module", choices=("kvm_amd", "kvm_intel"), required=True)
    parser.add_argument("--qemu", default="qemu-system-x86_64")
    parser.add_argument("--qemu-img", default="qemu-img")
    parser.add_argument("--bios")
    parser.add_argument("--firmware-dir")
    parser.add_argument("--work-dir", type=Path)
    parser.add_argument("--timeout", type=int, default=240)
    args = parser.parse_args()
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="release-host-") as temporary:
        scratch = Path(temporary)
        seed = make_seed(args, scratch)
        disk = scratch / "worker-host.qcow2"
        subprocess.run([args.qemu_img, "create", "-f", "qcow2", "-F", "raw", "-b",
                        args.base_image, disk, "5G"], check=True)
        installed = boot(args, seed, disk, "upgraded")
        recovered = boot(args, seed, disk, "rebooted")
        assert installed != recovered, (installed, recovered)
        print(f"Systemd installation survived a virtual host power cut: {installed} -> {recovered}", flush=True)


if __name__ == "__main__":
    main()
