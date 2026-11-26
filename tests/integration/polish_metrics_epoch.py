"""Exercise passive public observations through automatic sleep and a new execution."""

from datetime import datetime
import json
from pathlib import Path
import time

from authorized_services_kvm import (
    PROGRAM, SERVICE_PORT, create_service, service_client, worker_get, write_service,
)
from managed_system_vsock import wait_for


def execution_identity(view, ref):
    return dict(binding_id=view["binding"]["binding_id"], binding_generation=view["binding_generation"],
                incarnation_id=ref["incarnation_id"], execution_epoch=ref["execution_epoch"])


def assert_sample(sample, identity):
    assert sample["availability"] == "partial" and sample["source"] == "cgroup_v2" and sample["scope"] == "machine", sample
    assert sample["execution"] == identity, (sample, identity)
    assert isinstance(sample["cpu_millis"], int) and sample["cpu_millis"] >= 0 and sample["memory_mb"] > 0, sample
    assert sample.get("rss_mb") is None, sample
    observed = datetime.fromisoformat(sample["observed_at"])
    assert observed.tzinfo is not None, sample


def assert_unavailable(sample):
    assert sample["availability"] == "unavailable" and sample["scope"] == "machine" and sample["issues"], sample
    assert all(sample.get(field) is None for field in (
        "cpu_millis", "cpu_limit_millicpus", "vcpus", "memory_mb", "memory_limit_mb", "rss_mb",
        "disk_used_mb", "disk_gb", "egress_bytes", "observed_at", "execution",
    )), sample


def bracket_sample(platform, path, view, ref):
    # An independent host reading bounds each execution's cumulative CPU time.
    inventory = platform.native(f"/instances/{ref['instance_id']}/inventory")
    pid = inventory["execution"]["pid"]
    group, = [line.removeprefix("0::/") for line in Path(f"/proc/{pid}/cgroup").read_text().splitlines()
              if line.startswith("0::/")]
    scope = Path("/sys/fs/cgroup") / group

    def cpu_millis():
        values = dict(line.split() for line in (scope / "cpu.stat").read_text().splitlines())
        return int(values["usage_usec"]) // 1000

    low = cpu_millis()
    sample = platform.app(path + "/metrics")
    high = cpu_millis()
    assert_sample(sample, execution_identity(view, ref))
    assert low <= sample["cpu_millis"] <= high, (low, sample, high)
    return sample


def check_passive_idle_epoch(platform, path, ref):
    """Run on a started System fixture with a private uplink and an HTTP apps domain."""
    mid = path.removeprefix("machines/")
    lifecycle = path + "/lifecycle"
    policy = platform.app(lifecycle)
    assert policy["supported"], ("fixture requires a Bundle qualified for the lifecycle policy", policy)
    view = platform.app(path)
    binding = view["binding"]
    scope = dict(machine_id=mid, generation=binding["binding_generation"], task_namespace=binding["task_namespace"],
                 nonce=binding["binding_id"])
    bound = json.loads(platform.guest(ref, ["/usr/local/bin/taskctl", "rpc"],
                                      json.dumps(dict(method="bind", binding=scope))))
    assert bound["binding"] == scope, bound

    write_service(platform, ref)
    platform.guest(ref, ["systemd-run", "--unit=polish-passive-service", "/usr/bin/perl", PROGRAM, "http", str(SERVICE_PORT)])
    wait_for(lambda: f":{SERVICE_PORT:04X}".encode() in platform.guest(ref, ["cat", "/proc/net/tcp"]),
             timeout=25, retry=(RuntimeError, OSError))
    sid = create_service(platform, mid, "http", SERVICE_PORT)
    service_path = f"services/{sid}"
    configured = platform.app(service_path)
    assert configured["observation"]["status"] == "unknown" and configured["observation"].get("observed_at") is None, configured
    invoke = service_client(platform, mid, "http", sid, None, None, None)
    assert invoke("polish-before-idle") == (200, None, b"polish-before-idle\n")
    seed = platform.app(service_path)["observation"]
    assert seed["status"] == "ready" and seed["scope"] == "worker_upstream" and seed["execution_epoch"] == ref["execution_epoch"], seed
    assert seed["observed_at"], seed

    initial = bracket_sample(platform, path, view, ref)
    original_identity = initial["execution"]

    def ready_collectors():
        status = worker_get(platform, "GetLifecycleStatus", ref)
        collectors = status["collectors"]
        if status["power"] == "RUNNING" and not status["blockers"] and len(collectors) == 3 and all(
                item["fresh"] for item in collectors.values()):
            return status

    wait_for(ready_collectors, timeout=45)
    prior_events = worker_get(platform, "QueryLifecycleEvents", ref, {"after": 0})["events"]
    cursor = max((event["seq"] for event in prior_events), default=0)

    def configure(key, values):
        current = platform.app(lifecycle)
        op = platform.change(path, key, {"expected_policy_revision": current["desired_revision"], "lifecycle_policy": values},
                             platform.app(path), method="PATCH")
        platform.complete(op)
        effective = platform.app(lifecycle)
        assert effective["desired_revision"] == effective["applied_revision"] > current["desired_revision"], effective
        assert all(effective["applied"][field] == want for field, want in values.items()), effective

    configure("polish-passive-idle", dict(auto_standby=True, standby_after_seconds=4, auto_hibernate=True,
                                          hibernate_after_seconds=14, min_running_after_wake_seconds=0, pressure_reclaim="disabled"))
    samples = dict(running=0, standby=0, hibernated=0, stopped=0)

    def passive_poll():
        listed = platform.app("machines")["items"]
        assert any(machine["machine_id"] == mid for machine in listed), listed
        power = platform.app(path)["power_state"]
        sample = platform.app(path + "/metrics")
        assert sample["platform"]["active_access_holds"] == 0 and sample["platform"]["wake_waiters"] == 0, sample
        service = platform.app(service_path)
        observation = service["observation"]
        assert observation["observed_at"] == seed["observed_at"], (seed, service)
        collection = platform.app(path + "/services")["items"]
        match, = [row for row in collection if row["service_id"] == sid]
        assert match["observation"]["observed_at"] == seed["observed_at"], (seed, match)
        if power in samples:
            samples[power] += 1
        # Power can change between separate GETs. A usable sample still belongs
        # to the original execution regardless of the current power projection.
        if sample["availability"] != "unavailable":
            assert_sample(sample, original_identity)
        return power, sample, observation

    def saved():
        power, sample, observation = passive_poll()
        if power != "hibernated":
            return None
        assert_unavailable(sample)
        assert observation["status"] == "stale" and observation["execution_epoch"] == ref["execution_epoch"], observation
        return True

    try:
        wait_for(saved, timeout=80)
    except TimeoutError:
        public = platform.app(path)
        current = ref if public["power_state"] in ("running", "standby") else {**ref, "incarnation_id": None}
        print(json.dumps(dict(case="idle_deadline", samples=samples, public_power=public["power_state"],
                              worker=worker_get(platform, "GetLifecycleStatus", current),
                              events=worker_get(platform, "QueryLifecycleEvents", current, {"after": cursor}))),
              flush=True)
        raise
    assert samples["running"] >= 2 and samples["standby"] >= 2, samples
    for _ in range(5):
        assert saved(), samples
        time.sleep(.2)

    offline_ref = {**ref, "incarnation_id": None}
    hibernated = worker_get(platform, "GetLifecycleStatus", offline_ref)
    assert hibernated["power"] == "HIBERNATED" and hibernated["statepoint"] and not hibernated["incarnation_id"], hibernated
    events = worker_get(platform, "QueryLifecycleEvents", offline_ref, {"after": cursor})["events"]
    completed = [event for event in events if event["origin"] == "idle" and event["stage"] == "completed"]
    assert [event["kind"] for event in completed] == ["standby", "hibernate"], events
    assert not any(event["origin"] == "wake" for event in events), events

    configure("polish-passive-idle-off", dict(auto_standby=False, auto_hibernate=False))
    assert saved(), "applying idle settings woke the saved execution"
    wake = platform.complete(platform.change(path + "/start", "polish-passive-explicit-wake", {}, platform.app(path)))
    assert wake["result"]["statepoint"]["capture_id"] == hibernated["statepoint"], wake
    live_view, live_ref = platform.context(mid)
    assert live_ref["instance_id"] == ref["instance_id"] and live_ref["incarnation_id"] != ref["incarnation_id"], (ref, live_ref)
    assert live_ref["execution_epoch"] == ref["execution_epoch"] + 1, (ref, live_ref)
    next_sample = bracket_sample(platform, path, live_view, live_ref)
    assert next_sample["execution"] != original_identity
    assert datetime.fromisoformat(next_sample["observed_at"]) > datetime.fromisoformat(initial["observed_at"])
    stale = platform.app(service_path)["observation"]
    assert stale["status"] == "stale" and stale["observed_at"] == seed["observed_at"], (seed, stale)
    assert stale["execution_epoch"] == ref["execution_epoch"], stale
    assert invoke("polish-after-wake") == (200, None, b"polish-after-wake\n")
    verified = platform.app(service_path)["observation"]
    assert verified["status"] == "ready" and verified["execution_epoch"] == live_ref["execution_epoch"], verified
    assert datetime.fromisoformat(verified["observed_at"]) > datetime.fromisoformat(seed["observed_at"]), verified

    stopped = platform.complete(platform.change(path + "/stop", "polish-passive-cold-stop", {},
                                               platform.wait_capability(path, "lifecycle.stop")))
    for _ in range(3):
        assert platform.app(path)["power_state"] == "stopped"
        assert_unavailable(platform.app(path + "/metrics"))
        samples["stopped"] += 1
        time.sleep(.2)
    stopped_status = worker_get(platform, "GetLifecycleStatus", {**live_ref, "incarnation_id": None})
    assert stopped_status["power"] == "STOPPED" and not stopped_status["incarnation_id"], stopped_status
    print(json.dumps(dict(case="passive_idle_metrics_epoch", bundle=platform.bundle_id, machine_id=mid,
                          polls=samples, idle_operations=[event["operation_id"] for event in completed],
                          saved_capture=hibernated["statepoint"], original=original_identity, restored=next_sample["execution"],
                          cpu_millis=[initial["cpu_millis"], next_sample["cpu_millis"]],
                          service_observed_before=seed["observed_at"], service_observed_after=verified["observed_at"],
                          stop_operation_id=stopped["operation_id"])), flush=True)
