"""Check guarded power, leases and workspace settings on the CLI terminal fixture."""

from datetime import datetime
import json
import subprocess

from authorized_services_kvm import worker_get
from managed_system_vsock import wait_for


def check(platform, args, mid, env):
    path = f"machines/{mid}"
    cli_env = dict(env)
    cli_env.pop("SANDBOX_LEASE_TOKEN", None)

    def call(*command, expected=0, json_mode=True, lease_token=None):
        env_for_call = cli_env if lease_token is None else {**cli_env, "SANDBOX_LEASE_TOKEN": lease_token}
        flags = ["--json"] if json_mode else []
        proc = subprocess.run([args.sandboxctl, *flags, *command], env=env_for_call,
                              stdin=subprocess.DEVNULL, capture_output=True, check=False, timeout=220)
        if proc.returncode != expected:
            # Acquire can return a token even when a later check fails. Keep all
            # lease subprocess output private, including malformed responses.
            detail = None
            if command[0] != "leases":
                detail = proc.stderr.decode(errors="replace")[:1200]
                bearer = cli_env.get("SANDBOX_TOKEN")
                if bearer:
                    detail = detail.replace(bearer, "<redacted>")
            raise AssertionError({"command": command, "expected_exit": expected, "exit": proc.returncode, "error": detail})
        if not json_mode:
            return proc.stdout
        try:
            return json.loads(proc.stderr if expected else proc.stdout)
        except (UnicodeError, ValueError):
            raise AssertionError(f"{command[:2]} did not return a JSON receipt") from None

    def op(*command):
        receipt = call(*command)
        assert receipt["state"] == "succeeded" and receipt["effect"] == "applied", receipt
        assert receipt["operation_id"], receipt
        return receipt

    def guest(*command):
        return call("exec", "--follow", "--cwd", "/", "--timeout-seconds", "30", mid, "--", *command,
                    json_mode=False)

    def execution():
        view = call("machines", "show", mid)
        assert view["power_state"] == "running", view
        b = view["binding"]
        ref = platform.native(f"/machines/{b['runtime_name']}/execution")
        assert (b["runtime_instance_object_id"], b["incarnation_id"], b["execution_epoch"]) == (
            ref["instance_id"], ref["incarnation_id"], ref["execution_epoch"],
        ), (b, ref)
        return view, ref

    def idle_access():
        def cleared():
            active = call("machines", "metrics", mid)["platform"]
            return active["active_access_holds"] == active["wake_waiters"] == 0

        wait_for(cleared, timeout=30)

    def refused(*command):
        result = call("machines", *command, expected=1)
        error = result["error"]
        assert (result["status"], error["code"], error["effect"]) == (409, "STATE_CONFLICT", "none"), result
        assert not error.get("operation_id"), result
        return error["code"]

    view, first = execution()
    policy = call("lifecycle", "status", mid)
    assert policy["supported"] and policy["desired_revision"] == policy["applied_revision"] > 0, policy
    assert policy["worker"]["execution_epoch"] == first["execution_epoch"], policy
    if policy["applied"]["auto_standby"] or policy["applied"]["auto_hibernate"]:
        op("lifecycle", "set", mid, "--policy", '{"auto_standby":false,"auto_hibernate":false}')
    original_boot = guest("/bin/cat", "/proc/sys/kernel/random/boot_id").strip()
    assert original_boot and guest("/usr/bin/stat", "-f", "-c", "%T", "/run").strip() == b"tmpfs"
    volatile = "/run/polish-cli-power"
    persistent = "/root/polish-cli-power"
    marker = f"polish-cli-power:{mid}"
    guest("/bin/sh", "-ec", 'printf %s "$1" >"$2"; printf %s "$1" >"$3"; sync "$3"',
          "polish-cli-power", marker, volatile, persistent)
    idle_access()

    wrong_wake = refused("wake-full", mid, "--from", "hibernated")
    assert execution()[1] == first
    standby = op("machines", "standby", mid)
    parked = call("machines", "show", mid)
    assert parked["power_state"] == "standby", parked
    assert parked["binding"]["execution_epoch"] == first["execution_epoch"], parked
    assert refused("cold-start", mid) == "STATE_CONFLICT"
    assert worker_get(platform, "GetLifecycleStatus", first)["power"] == "STANDBY"
    wake = op("machines", "wake-full", mid, "--from", "standby")
    assert wake["kind"] == "wake" and execution()[1] == first, wake
    assert guest("/bin/cat", volatile, persistent) == (marker * 2).encode()
    assert guest("/bin/cat", "/proc/sys/kernel/random/boot_id").strip() == original_boot

    idle_access()
    stopped = op("machines", "stop", mid)
    assert stopped["source_disposition"] == "EXITED", stopped
    assert call("machines", "show", mid)["power_state"] == "stopped"
    offline = worker_get(platform, "GetLifecycleStatus", {**first, "incarnation_id": None})
    assert offline["power"] == "STOPPED" and not offline["incarnation_id"], offline
    assert offline["execution_epoch"] == first["execution_epoch"], offline
    started = op("machines", "cold-start", mid)
    assert started["kind"] == "start" and not started["result"].get("statepoint"), started

    def ready():
        machine = call("machines", "show", mid)
        return machine["power_state"] == "running" and machine["guest_management_status"] == "ready"

    wait_for(ready, timeout=60)
    after, current = execution()
    assert (after["binding"]["binding_id"], after["binding_generation"]) == (
        view["binding"]["binding_id"], view["binding_generation"],
    ), after
    assert current["instance_id"] == first["instance_id"] and current["incarnation_id"] != first["incarnation_id"], (first, current)
    assert current["execution_epoch"] == first["execution_epoch"] + 1, (first, current)
    next_boot = guest("/bin/cat", "/proc/sys/kernel/random/boot_id").strip()
    assert next_boot and next_boot != original_boot
    assert guest("/bin/sh", "-ec", 'test ! -e "$1"; cat "$2"', "polish-cli-power", volatile, persistent) == marker.encode()

    reason = "CLI scoped v2 proof"
    acquired = call("leases", "acquire", mid, "--semantics-version", "2", "--ttl-seconds", "60", "--reason", reason)
    lease_token = acquired.pop("lease_token", None)
    assert lease_token, "acquire returned no lease token"
    lease = acquired["lease_id"]

    def listed(state):
        row, = [item for item in call("leases", "list", mid)["items"] if item["lease_id"] == lease]
        assert not row.get("lease_token"), "list exposed the lease token"
        assert (row["machine_id"], row["semantics_version"], row["execution_epoch"], row["state"]) == (
            mid, 2, current["execution_epoch"], state,
        ), row
        return row

    first_lease = listed("active")
    assert first_lease["revision"] == acquired["revision"], first_lease
    status = call("lifecycle", "status", mid)
    assert status["worker"]["execution_epoch"] == current["execution_epoch"] and f"lease:{reason}" in status["worker"]["blockers"], status
    renewed = call("leases", "renew", lease, "--revision", str(first_lease["revision"]), "--ttl-seconds", "180",
                   lease_token=lease_token)
    assert not renewed.get("lease_token"), "renew exposed the lease token"
    assert renewed["revision"] > first_lease["revision"] and renewed["state"] == "active", renewed
    assert datetime.fromisoformat(renewed["expires_at"]) > datetime.fromisoformat(first_lease["expires_at"]), renewed
    assert listed("active")["revision"] == renewed["revision"]
    released = call("leases", "release", lease, "--revision", str(renewed["revision"]), lease_token=lease_token)
    assert not released.get("lease_token"), "release exposed the lease token"
    assert released["revision"] > renewed["revision"] and released["state"] == "released", released
    assert listed("released")["revision"] == released["revision"]

    name = "CLI power " + mid[-12:]
    renamed = op("machines", "rename", mid, name)
    named, = [row for row in call("machines", "list")["items"] if row["name"] == name]
    assert named["machine_id"] == mid, named
    by_name = call("machines", "show", "name:" + name)
    assert (by_name["machine_id"], by_name["name"]) == (mid, name), by_name
    config = call("machines", "config", "get", "name:" + name)
    assert config["machine_id"] == mid and config["resources"]["cpu_count"] > 0, config
    resources = call("machines", "resources", mid)
    initial_limit = resources.get("cpu_limit")
    resource_evidence = {"status": "unavailable", "issue": resources.get("issues", {}).get("cpu_limit")}
    if initial_limit is not None:
        limit = 500 if initial_limit["requested_millicpus"] != 500 else 750
        def set_limit(value):
            return op("machines", "config", "set", mid,
                      "--body", json.dumps({"resources": {"cpu_limit_millicpus": value}}))

        changed = set_limit(limit)
        applied = call("machines", "resources", mid)["cpu_limit"]
        assert applied["requested_millicpus"] == limit and applied["incarnation_id"] == current["incarnation_id"], applied
        assert applied["quota_usec"] * 1000 == limit * applied["period_usec"], applied
        restored = set_limit(initial_limit["requested_millicpus"])
        assert call("machines", "resources", mid)["cpu_limit"] == initial_limit
        resource_evidence = {"status": "verified", "set_operation": changed["operation_id"],
                             "restore_operation": restored["operation_id"], "worker_readback": applied}
    assert execution()[1] == current
    print(json.dumps(dict(case="cli-power-policy", machine_id=mid, bundle=platform.bundle_id,
                          wrong_wake=wrong_wake, before=first, after=current, boot_changed=next_boot != original_boot,
                          warm_wake_preserved_ram=True, cold_start_cleared_ram=True, disk_preserved=True,
                          standby_operation=standby["operation_id"], warm_wake_operation=wake["operation_id"],
                          stop_operation=stopped["operation_id"], cold_start_operation=started["operation_id"],
                          lease_id=lease, lease_revisions=[first_lease["revision"], renewed["revision"], released["revision"]],
                          rename_operation=renamed["operation_id"], unique_name=name, resources=resource_evidence)), flush=True)
