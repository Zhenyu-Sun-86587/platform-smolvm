"""Run with SANDBOX_URL, SANDBOX_TOKEN, SANDBOX_TEMPLATE, and SANDBOX_NAME set."""
import codecs
import os
import sys
from pathlib import Path
from time import monotonic, sleep

from sandbox_platform import Client


def success(client, operation):
    print(f"Observing {operation['operation_id']}", file=sys.stderr)
    result = client.wait_operation(operation["operation_id"])
    if result["state"] != "succeeded":
        raise RuntimeError(result)
    return result["result"]


def main():
    url, token, template, name = (
        os.environ[key] for key in ("SANDBOX_URL", "SANDBOX_TOKEN", "SANDBOX_TEMPLATE", "SANDBOX_NAME")
    )
    with Client(url, token) as client:
        created = success(client, client.create_machine({
            "name": name, "template_id": template, "start": True,
            "resources": {"cpu_count": 1, "memory_mib": 1024, "storage_gib": 4, "overlay_gib": 4},
            "idle_policy": {"enabled": False, "timeout_seconds": 1800},
        }))
        machine_id = created["machine_id"]
        task = success(client, client.start_task(machine_id, {
            "argv": ["/bin/sh", "-c", "for n in 1 2 3; do printf 'step %s\\n' \"$n\"; sleep 2; done"],
        }))
        task_id = task["task_id"]
        print(f"Persisted {machine_id} / {task_id}", file=sys.stderr)

    offset = 0
    decoder = codecs.getincrementaldecoder("utf-8")(errors="replace")
    with Client(url, token) as client:
        output = client.task_output(task_id, offset=offset)
        for _, data, offset, gap in output:
            if gap:
                print(f"Output gap before offset {offset}", file=sys.stderr)
            print(decoder.decode(data), end="")
            break
        output.close()
    with Client(url, token) as client:
        for _, data, offset, gap in client.task_output(task_id, offset=offset):
            if gap:
                print(f"Output gap before offset {offset}", file=sys.stderr)
            print(decoder.decode(data), end="")
        print(decoder.decode(b"", final=True), end="")
        result = client.get_task(task_id)
        if result.get("exit_code") != 0:
            raise RuntimeError(result)
        snapshot = success(client, client.capture(machine_id, "clone", "After task"))
        print(f"Snapshot {snapshot['snapshot_id']}")
        clone = success(client, client.clone(machine_id, name + "-clone"))
        print(f"Clone {clone['machine_id']}")
        if os.environ.get("SANDBOX_EXAMPLE_SERVICE") == "1":
            probe_service(client, clone["machine_id"], token)


def probe_service(client, machine_id, token):
    program = Path(__file__).with_name("service.pl").read_text()
    running = success(client, client.start_task(machine_id, {
        "argv": ["/usr/bin/perl", "-e", program],
    }))
    task_id = running["task_id"]
    try:
        published = success(client, client.expose(machine_id, {
            "name": "workspace-http", "protocol": "http", "guest_port": 8080,
        }))
        service = client.get("services/" + published["service_id"])
        uri = "https://" + service["public_host"] + "/"
        # Avoid automatic redirects so this example keeps the credential on the service origin.
        import httpx

        with httpx.Client(follow_redirects=False) as http:
            deadline = monotonic() + 30
            while True:
                response = http.get(uri, headers={"Authorization": "Bearer " + token})
                if response.is_success:
                    if response.text != "Sandbox workspace ready\n":
                        raise RuntimeError("unexpected service response")
                    print(f"Service {uri} returned HTTP {response.status_code}")
                    break
                if response.status_code not in (409, 502, 503) or monotonic() >= deadline:
                    response.raise_for_status()
                sleep(0.4)
    finally:
        success(client, client.cancel_task(task_id, grace_seconds=0))


if __name__ == "__main__":
    main()
