#!/usr/bin/env python3
"""Verify application RAM and both actual disk journals across installed KVM captures."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import random
import sqlite3
import subprocess
import sys
import tempfile
import time

from installed_fork_backing_kvm import fence_and_remove, guest_lines, invoke_fork
from installed_fork_lineage_kvm import attached_volumes, native


PHASES = ("prepared", "writing-first", "split", "writing-second", "committed")
REQUIRED_CUTS = ("prepared", "writing-first", "left-ahead", "right-ahead", "writing-second", "committed")


def frames(root, reference, prefix):
    samples = []
    for line in guest_lines(root, reference, prefix + " "):
        fields = line.split()
        if len(fields) != 8:
            continue
        _, pid, hostname, transaction, phase, first_disk, left, right = fields
        number, stage, first = int(transaction), int(phase), int(first_disk)
        disks = [int(left), int(right)]
        assert first == number % 2 and 0 <= stage < len(PHASES), line
        first_count, second_count = disks[first], disks[first ^ 1]
        prior = number - 1
        legal = (
            (first_count, second_count) == (prior, prior),
            first_count in (prior, number) and second_count == prior,
            (first_count, second_count) == (number, prior),
            first_count == number and second_count in (prior, number),
            (first_count, second_count) == (number, number),
        )
        assert legal[stage], line
        samples.append({"pid": int(pid), "hostname": hostname, "transaction": number,
                        "ram_phase": PHASES[stage], "first_disk": first,
                        "disk0_counter": disks[0], "disk1_counter": disks[1]})
    return samples


def label(frame):
    if frame["ram_phase"] == "split":
        return "right-ahead" if frame["first_disk"] else "left-ahead"
    return frame["ram_phase"]


def key(frame):
    return frame["transaction"], PHASES.index(frame["ram_phase"])


def matches_cut(original, resumed):
    assert resumed["pid"] == original["pid"] and key(resumed) == key(original), (original, resumed)
    assert resumed["first_disk"] == original["first_disk"], (original, resumed)
    before = original["disk0_counter"], original["disk1_counter"]
    after = resumed["disk0_counter"], resumed["disk1_counter"]
    stage = original["ram_phase"]
    if stage in ("writing-first", "writing-second"):
        # The pending write can finish while the devices are settling.
        assert all(a >= b for a, b in zip(after, before)), (original, resumed)
    else:
        assert after == before, (original, resumed)


def generation_inventory(root, operation):
    path = root / "catalog/catalog.sqlite"
    with sqlite3.connect(f"file:{path}?mode=ro", uri=True) as reader:
        state, plan = reader.execute("SELECT state,plan_json FROM writer_pivots WHERE operation_id=?",
                                     (operation,)).fetchone()
    assert state == "complete"
    heads = json.loads(plan)["heads"]
    assert len(heads) == 2
    return [{"volume": head["volume_id"], "sealed_generation": head["head_resource_id"],
             "new_writer_generation": head["next_resource_id"]} for head in heads]


def verify(launch, endpoint, root, source, seed, guest_digest):
    capabilities = launch.http(endpoint, launch.EXT + "/capabilities")
    serve = capabilities["runtime_instance_id"]
    bundle = capabilities["bundle_digest"]
    randomizer = random.Random(seed)

    def current():
        rows = frames(root, source, "A09_PHASE")
        return rows[-1] if rows else None

    def arm(wanted):
        def observed():
            latest = current()
            if latest and (wanted is None or label(latest) == wanted):
                return latest

        launch.until(observed, 30)
        delay = randomizer.uniform(.08, .25) if wanted else randomizer.uniform(.12, 4.8)
        time.sleep(delay)
        return delay * 1000

    def standby(wanted, operation):
        delay = arm(wanted)
        parked = native(launch, endpoint, "standby", operation, source, {"mode": "managed"})
        assert parked["native"]["source_disposition"] == "STANDBY"
        return current(), delay

    def first_and_progress(reference, expected_name, original):
        received = launch.until(lambda: frames(root, reference, "A09_RESUMED"), 20)[0]
        assert received["hostname"] == expected_name, received
        matches_cut(original, received)
        later = launch.until(lambda: next((sample for sample in frames(root, reference, "A09_PHASE")
                                           if key(sample) > key(received)), None), 20)
        assert later["pid"] == received["pid"]
        return received, later

    randomized = list(REQUIRED_CUTS)
    randomizer.shuffle(randomized)
    trials = []

    def fork_once(wanted):
        number = len(trials)
        captured, delay = standby(wanted, f"a09-park-{number}")
        child_name = f"a09-child-{number}"
        op = f"a09-cut-{number}"
        cut = invoke_fork(launch, endpoint, source, op, [child_name])
        assert cut["original_source_disposition"] == "STANDBY"
        child, = cut["children"]
        initial, later = first_and_progress(child, child_name, captured)
        assert current() == captured, (captured, current())
        generations = generation_inventory(root, op)
        fence_and_remove(launch, endpoint, root, serve, child_name, child)
        wake = native(launch, endpoint, "wake", f"a09-wake-{number}", source, {})
        assert wake["native"]["source_disposition"] == "RUNNING"
        trials.append({"requested_phase": wanted, "observed_phase": label(initial),
                       "sample_delay_ms": round(delay, 1), "capture": cut["capture_id"],
                       "source_at_cut": captured, "child_first_read": initial, "child_continued": later,
                       "writer_pivot": generations})
        return label(initial)

    covered = set()
    for wanted in randomized:
        for _ in range(3):
            if wanted in covered:
                break
            covered.add(fork_once(wanted))
    covered.update(fork_once(None) for _ in range(2))
    assert set(REQUIRED_CUTS) <= covered, (randomized, covered)
    assert len({row["capture"] for row in trials}) == len(trials)

    # Exercise the durable reader at an unequal, already verified application phase.
    captured, delay = standby("left-ahead", "a09-park-durable")
    assert captured["ram_phase"] == "split", captured
    volumes = attached_volumes(launch, endpoint, source)
    saved = native(launch, endpoint, "hibernate", "a09-hibernate-split", source, {"capture_id": "a09-split"})
    assert saved["statepoint"]["phase"] == "hibernated" and saved["statepoint"]["source_released"]
    assert current() == captured
    binding = launch.http(endpoint, launch.EXT + "/machines/test-managed/binding")
    assert binding["incarnation_id"] is None
    restored = native(launch, endpoint, "vm_restore", "a09-restore-split", binding,
                      {"name": "test-managed", "capture_id": "a09-split", "mode": "exact"})
    successor = restored["execution"]
    assert successor["execution_epoch"] == source["execution_epoch"] + 1
    assert restored["statepoint"]["manifest_digest"] == saved["statepoint"]["manifest_digest"]
    assert attached_volumes(launch, endpoint, successor) == volumes
    initial, later = first_and_progress(successor, "test-managed", captured)
    committed = launch.until(lambda: next((sample for sample in frames(root, successor, "A09_PHASE")
                                           if sample["transaction"] == initial["transaction"]
                                           and sample["ram_phase"] == "committed"), None), 20)
    assert committed["pid"] == initial["pid"]

    print(json.dumps({
        "case": "A09", "bundle": bundle, "guest_fixture_sha256": guest_digest,
        "seed": seed, "requested_order": randomized, "observed_phases": sorted(covered),
        "volatile_cuts": trials,
        "durable_cut": {"sample_delay_ms": round(delay, 1), "source_at_cut": captured,
                        "first_read": initial, "continued": later,
                        "committed_original_transaction": committed,
                        "manifest_digest": saved["statepoint"]["manifest_digest"],
                        "writer_pivot": generation_inventory(root, "a09-hibernate-split"),
                        "restored_volumes": volumes},
    }, sort_keys=True), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("bundle", "smolvm", "guest", "work-dir"):
        parser.add_argument("--" + name, required=True, type=lambda path: Path(path).resolve(strict=True))
    parser.add_argument("--seed", type=int, default=92077)
    args = parser.parse_args()
    if os.geteuid() != 0:
        parser.error("requires root, Linux KVM and a systemd manager")
    if os.environ.get("MULTIDISK_PHASE_NAMESPACE") != "1":
        os.execvpe("unshare", ["unshare", "--mount", "--propagation", "private", "--net", "--",
                              sys.executable, __file__, *sys.argv[1:]],
                   {**os.environ, "MULTIDISK_PHASE_NAMESPACE": "1"})
    sys.path.insert(0, str(args.smolvm / "tests"))
    import test_deep_managed_launch as launch

    scope = f"smolvmdiskphase{os.getpid()}_{os.urandom(4).hex()}.slice"
    subprocess.run(["busctl", "call", "org.freedesktop.systemd1", "/org/freedesktop/systemd1",
                    "org.freedesktop.systemd1.Manager", "StartTransientUnit", "ssa(sv)a(sa(sv))",
                    scope, "fail", "1", "MemoryAccounting", "b", "true", "0"],
                   check=True, stdout=subprocess.DEVNULL)
    os.environ["SMOLVM_WORKER_SLICE_NAME"] = scope
    try:
        with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="multidisk-phase-") as directory:
            root = Path(directory)
            root.chmod(0o755)
            with args.guest.open("rb") as guest:
                digest = hashlib.file_digest(guest, "sha256").hexdigest()

            def observer(endpoint, scratch, source, _inventory, _started):
                verify(launch, endpoint, scratch, source, args.seed, digest)

            try:
                launch.verify(args.bundle, root, args.guest, None, False, False, False, False,
                              observer=observer)
            except BaseException:
                for log in (root / "worker.log", root / "serve.log", *sorted((root / "state/managed").glob("*.log"))):
                    if log.is_file():
                        print(f"{log.name}\n{log.read_text(errors='replace')[-2200:]}", file=sys.stderr)
                raise
    finally:
        subprocess.run(["systemctl", "stop", scope], check=True)


if __name__ == "__main__":
    main()
