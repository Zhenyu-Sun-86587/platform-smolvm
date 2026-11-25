"""Exercise installed systemd services against a disposable managed VM."""
import argparse
import base64
import http.client
import json
import pathlib
import socket
import subprocess
import time
import uuid


class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX)
        self.sock.settimeout(self.timeout)
        self.sock.connect("/run/smolvm-platform/api.sock")


def wait(check, seconds=60):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        try:
            if value := check():
                return value
        except (OSError, http.client.HTTPException):
            pass
        time.sleep(0.2)
    raise TimeoutError(check)


def unit_pid(unit):
    return int(subprocess.check_output(["systemctl", "show", "-p", "MainPID", "--value", unit], text=True))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--password-file", type=pathlib.Path, required=True)
    parser.add_argument("--template")
    reboot = parser.add_mutually_exclusive_group()
    reboot.add_argument("--prepare-reboot", type=pathlib.Path, metavar="STATE_FILE")
    reboot.add_argument("--verify-reboot", type=pathlib.Path, metavar="STATE_FILE")
    args = parser.parse_args()
    if not args.verify_reboot and not args.template:
        parser.error("--template is required before preparing a fixture")
    if (args.prepare_reboot or args.verify_reboot) and socket.gethostname() != "sandbox-w11-reboot":
        parser.error("reboot tests require the sandbox-w11-reboot disposable system")
    address = "127.0.0.1:8443"
    headers = {"Origin": "http://" + address, "Content-Type": "application/json"}

    def request(method, path, body=None, fields=None):
        conn = http.client.HTTPConnection(address, timeout=20)
        try:
            conn.request(method, "/v1/" + path, None if body is None else json.dumps(body), headers | (fields or {}))
            reply = conn.getresponse()
            raw = reply.read()
            if reply.status not in (200, 202):
                raise AssertionError((method, path, reply.status, raw[:450]))
            return reply, json.loads(raw) if raw else None
        finally:
            conn.close()

    def get(path):
        return request("GET", path)[1]

    reply, login = request("POST", "auth/login", {"username": "owner", "password": args.password_file.read_text().strip()})
    headers.update({"Cookie": reply.getheader("Set-Cookie").split(";")[0], "X-CSRF-Token": login["csrf_token"]})
    control = get("control")
    headers["If-Control-Epoch"] = control["control_epoch"]

    def mutate(method, path, body, machine=None):
        fields = {"Idempotency-Key": uuid.uuid4().hex}
        if machine:
            view = get("machines/" + machine)
            fields["If-Match"] = f'"{view["revision"]}"'
            if not path.startswith("tasks/"):
                body["expected_generation"] = view["binding_generation"]
        _, accepted = request(method, path, body, fields)

        def result():
            op = get("operations/" + accepted["operation_id"])
            if op["state"] in ("failed", "cancelled", "recovery_required"):
                raise AssertionError(op)
            return op if op["state"] == "succeeded" else None

        return wait(result, 120)

    def runtime_request(native, suffix=""):
        conn = UnixHTTP("localhost", timeout=3)
        try:
            conn.request("GET", "/api/v1/machines/" + native + suffix)
            response = conn.getresponse()
            assert response.status == 200, response.status
            return response.read()
        finally:
            conn.close()

    def boot_id():
        return pathlib.Path("/proc/sys/kernel/random/boot_id").read_text().strip()

    if args.verify_reboot:
        saved = json.loads(args.verify_reboot.read_text())
        assert boot_id() != saved["boot_id"], (boot_id(), saved["boot_id"])
        assert control["control_epoch"] == saved["epoch"]
        mid, task, native = saved["machine"], saved["task"], saved["runtime"]
        base = "machines/" + mid

        def reconciled():
            machine = get(base)
            view = get("tasks/" + task)
            return view if machine["observed_state"] == "stopped" and view["state"] in ("unknown", "interrupted") else None

        view = wait(reconciled)
        assert view["reason"] in ("host_boot_changed", "guest_cold_boot"), view
        assert get("operations/" + saved["task_operation"])["state"] == "succeeded"
        runtime = json.loads(runtime_request(native))
        assert runtime["state"] == "stopped" and "runtime" not in runtime
        print("host boot", saved["boot_id"], "->", boot_id(), "native state", runtime["state"],
              "task", task, view["state"], view["reason"], flush=True)
        mutate("POST", base + "/start", {}, mid)
        receipt = wait(lambda: item if (item := get("tasks/" + task))["state"] == "interrupted" else None)
        assert receipt["reason"] == "guest_cold_boot", receipt
        assert runtime_request(native, "/files/workspace/w11-start-count") == b"started\n"
        print("explicit cold start returned the original guest interruption receipt and one submission", flush=True)
        mutate("DELETE", base, {}, mid)
        return

    task = mid = None
    preserve = False
    try:
        created = mutate("POST", "machines", {"name": "installed-" + uuid.uuid4().hex[:10],
            "template_id": args.template, "start": True, "idle_policy": {"enabled": False}})
        mid = created["machine_id"]
        base = "machines/" + mid
        native = created["runtime_name"]
        command = "while :; do echo systemd-tick; sleep 1; done"
        if args.prepare_reboot:
            command = "set -e; echo started >>/workspace/w11-start-count; sync -f /workspace/w11-start-count; " + command
        started = mutate("POST", base + "/tasks", {"argv": ["/bin/sh", "-c", command],
            "task_key": uuid.uuid4().hex}, mid)
        task = started["result"]["task_id"]

        def runtime_identity():
            return json.loads(runtime_request(native)).get("runtime")

        def output(offset):
            view = get(f"tasks/{task}/output?offset={offset}&stream=stdout")
            return view["next"] if base64.b64decode(view["data_base64"]) else None

        identity = runtime_identity()
        cgroup = pathlib.Path(f"/proc/{identity['pid']}/cgroup").read_text().strip()
        assert f"/smolvm-vm-{native}.scope" in cgroup and "/smolvm-serve.service" not in cgroup, cgroup
        cursor = wait(lambda: output(0))
        print("runtime", native, "identity", identity, "scope", cgroup, "task", task, "cursor", cursor, flush=True)
        if args.prepare_reboot:
            dropin = pathlib.Path("/run/systemd/system/smolvm-serve.service.d/99-test-stop-timeout.conf")
            dropin.parent.mkdir(parents=True, exist_ok=True)
            with dropin.open("x") as override:
                override.write("[Service]\nTimeoutStopSec=1s\nExecStop=/usr/bin/sleep 15\n")
            try:
                subprocess.run(["systemctl", "daemon-reload"], check=True)
                before = int(time.time())
                prior = unit_pid("smolvm-serve.service")
                subprocess.run(["systemctl", "restart", "smolvm-serve.service"], check=True)
                next_pid = wait(lambda: pid if (pid := unit_pid("smolvm-serve.service")) not in (0, prior) else None)
                logs = subprocess.check_output(["journalctl", "-u", "smolvm-serve.service",
                    "--since", f"@{before}", "--no-pager", "-o", "cat"], text=True)
                assert "timed out" in logs and "Failed with result 'timeout'" in logs, logs
                assert wait(runtime_identity) == identity
                next_cursor = wait(lambda: output(cursor))
                print("smolvm-serve systemd stop timeout", f"{prior} -> {next_pid}",
                      "same VMM", identity["pid"], f"cursor {cursor} -> {next_cursor}", flush=True)
            finally:
                dropin.unlink()
                subprocess.run(["systemctl", "daemon-reload"], check=True)
            with args.prepare_reboot.open("x") as state:
                json.dump({"boot_id": boot_id(), "epoch": control["control_epoch"], "machine": mid,
                    "task": task, "task_operation": started["operation_id"], "runtime": native}, state)
            preserve = True
            return
        for unit, action in [("platformd.service", "restart"), ("smolvm-serve.service", "restart"),
                             ("platformd.service", "SIGKILL"), ("smolvm-serve.service", "SIGKILL")]:
            old = unit_pid(unit)
            argv = ["systemctl", "restart", unit] if action == "restart" else [
                "systemctl", "kill", "--kill-whom=main", "--signal=SIGKILL", unit]
            subprocess.run(argv, check=True)
            pid = wait(lambda: current if (current := unit_pid(unit)) not in (0, old) else None)
            fresh = wait(lambda: get("control"))
            assert fresh["control_epoch"] == control["control_epoch"], fresh
            assert wait(runtime_identity) == identity
            next_cursor = wait(lambda: output(cursor))
            assert next_cursor > cursor
            print(unit, action, f"{old} -> {pid}", "same VMM", identity["pid"], f"cursor {cursor} -> {next_cursor}", flush=True)
            cursor = next_cursor
        mutate("POST", f"tasks/{task}/cancel", {"grace_seconds": 1}, mid)
        task = None
        capture = mutate("POST", base + "/snapshots", {"purpose": "clone", "label": "installed-systemd"}, mid)
        print("signed checkpoint", capture["result"]["snapshot_id"], flush=True)
    finally:
        if mid and task and not preserve:
            mutate("POST", f"tasks/{task}/cancel", {"grace_seconds": 1}, mid)
        if mid and not preserve:
            mutate("DELETE", "machines/" + mid, {}, mid)


if __name__ == "__main__":
    main()
