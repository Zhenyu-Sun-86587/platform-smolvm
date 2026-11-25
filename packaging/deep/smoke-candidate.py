#!/usr/bin/env python3
"""Boot the locked OCI runtime twice on KVM and record the observed profile."""

import argparse
from contextlib import contextmanager
import errno
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time

from bundle import BUILD_LOCK, BUILD_RECORD, MANIFEST, ROOTFS, check_runtime, verify_bundle
from bundle_inputs import BundleError, canonical_json, file_digest, read_json, require


def write_image(probe, path):
    layer = io.BytesIO()
    with tarfile.open(fileobj=layer, mode="w") as archive:
        executable = tarfile.TarInfo("probe")
        executable.mode = 0o755
        executable.size = probe.stat().st_size
        with probe.open("rb") as stream:
            archive.addfile(executable, stream)
    layer = layer.getvalue()
    config = {
        "architecture": "amd64",
        "os": "linux",
        "config": {"Cmd": ["/probe", "guest"]},
        "rootfs": {"type": "layers", "diff_ids": ["sha256:" + hashlib.sha256(layer).hexdigest()]},
    }
    manifest = [{"Config": "config.json", "RepoTags": ["localhost/deep-oci-probe:1"], "Layers": ["layer.tar"]}]
    with tarfile.open(path, "w") as archive:
        for name, data in (("config.json", canonical_json(config)), ("layer.tar", layer),
                           ("manifest.json", canonical_json(manifest))):
            member = tarfile.TarInfo(name)
            member.size = len(data)
            archive.addfile(member, io.BytesIO(data))


def cpu_identity():
    names = {"vendor_id", "cpu family", "model", "stepping", "flags"}
    first_cpu = Path("/proc/cpuinfo").read_text().split("\n\n", 1)[0]
    fields = {}
    for line in first_cpu.splitlines():
        key, _, value = line.partition(":")
        if key.strip() in names:
            fields[key.strip()] = " ".join(value.split())
    require(fields.keys() == names, "cannot identify the verified x86-64 CPU")
    text = "platform=linux/amd64\n" + "".join(f"{key}={value}\n" for key, value in fields.items())
    return {
        "vendor": fields["vendor_id"],
        "fingerprint": hashlib.sha256(text.encode()).hexdigest(),
    }


@contextmanager
def smoke_workspace():
    directory = Path(tempfile.mkdtemp(prefix="smol-deep-oci-"))
    try:
        yield directory
    finally:
        for attempt in range(5):
            try:
                shutil.rmtree(directory)
                break
            except OSError as error:
                # A teardown receipt can open the isolated database as the
                # ephemeral machine exits. Retry before removing that state.
                if attempt == 4 or error.errno not in {errno.ENOENT, errno.ENOTEMPTY}:
                    raise
                time.sleep(0.25)


def run_oci(artifacts, probe):
    # The VMM UID must traverse the staged bundle even if its build user has
    # an inaccessible home. The private mount holds only the test UID claims.
    with smoke_workspace() as root:
        root.chmod(0o755)
        for name in ("guest", "home", "state", "var-lib"):
            (root / name).mkdir()
            (root / name).chmod(0o755)
        for name in ("smolvm", "smolvm-bin"):
            shutil.copy2(artifacts / name, root / name)
        shutil.copytree(artifacts / "lib", root / "lib")
        (root / "lib").chmod(0o755)
        subprocess.run(["tar", "-xzf", str(artifacts / ROOTFS), "--no-overwrite-dir", "-C", str(root / "guest")], check=True)
        write_image(probe, root / "workload.tar")
        command = """
set -euo pipefail
root=$1
mount --bind "$root/var-lib" /var/lib
export HOME="$root/home" SMOLVM_AGENT_ROOTFS="$root/guest"
export SMOLVM_LIB_DIR="$root/lib" LD_LIBRARY_PATH="$root/lib"
registry=/var/lib/smolvm-custom/worker/uid-claims
for attempt in 1 2; do
    timeout --kill-after=3s 60s "$root/smolvm" --state-dir "$root/state" \
        machine run --cpus 2 --mem 512 --image "$root/workload.tar" -- /probe guest
    [[ $(stat -c '%a:%u' "$registry") == 700:0 ]]
done
"""
        result = subprocess.run(
            ["unshare", "--mount", "--propagation", "private", "--", "bash", "-c", command, "bash", str(root)],
            env={"PATH": "/usr/local/bin:/usr/bin:/bin", "LC_ALL": "C"},
            stdout=subprocess.PIPE, text=True, check=True,
        )
    launches = [json.loads(line) for line in result.stdout.splitlines() if line.strip()]
    require(len(launches) == 2, "expected two successful OCI guest probes")
    for launch in launches:
        launch["virtio_devices"].sort(key=lambda device: device["slot"])
        require(launch["virtio_devices"], "guest returned no virtio devices")
    require(launches[0] == launches[1], "OCI guest profiles differ across the two cold boots")
    return launches[0]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("artifacts", "probe", "output"):
        parser.add_argument(f"--{name}", required=True, type=Path)
    args = parser.parse_args()
    try:
        require(os.geteuid() == 0, "run the candidate smoke as root on the isolated KVM worker")
        artifacts = args.artifacts.resolve(strict=True)
        if (artifacts / MANIFEST).exists():
            checksum = verify_bundle(artifacts)
            print(f"Verified installed Bundle {checksum}")
        else:
            check_runtime(artifacts)
        kvm = json.loads(subprocess.run(
            [str(args.probe.resolve(strict=True)), "host"], capture_output=True, text=True, check=True,
        ).stdout)
        guest = run_oci(artifacts, args.probe)
        require(guest["kernel_release"] == read_json(artifacts / BUILD_LOCK)["kernel"]["release"],
                "the running kernel does not match the build lock")
        report = {
            "schema": "RuntimeOciBootEvidence/1",
            "build_lock_digest": "sha256:" + file_digest(artifacts / BUILD_LOCK)[0],
            "build_record_digest": "sha256:" + file_digest(artifacts / BUILD_RECORD)[0],
            "boot_count": 2,
            "kvm": kvm,
            "host_cpu": cpu_identity(),
            "guest": guest,
        }
        with args.output.open("x", encoding="utf-8") as stream:
            json.dump(report, stream, indent=2, sort_keys=True)
            stream.write("\n")
        print(f"Verified two OCI boots and recorded the profile at {args.output}")
    except (BundleError, OSError, ValueError, subprocess.CalledProcessError, tarfile.TarError) as error:
        sys.exit(f"deep OCI smoke: {error}")


if __name__ == "__main__":
    main()
