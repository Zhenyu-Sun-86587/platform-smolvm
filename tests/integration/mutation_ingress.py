"""Verify service and credential mutation contracts on a disposable local platform."""

import argparse
import concurrent.futures
from contextlib import closing
from http.client import HTTPSConnection
import json
from pathlib import Path
import socket
import ssl
import tempfile
import time
from urllib.parse import urlsplit
from uuid import uuid4


class Platform:
    def __init__(self, origin, ca, username, password):
        self.origin = origin
        self.address = urlsplit(origin)
        self.tls = ssl.create_default_context(cafile=ca)
        self.headers = {"Origin": origin, "Content-Type": "application/json"}
        reply, data = self.call("POST", "auth/login", {"username": username, "password": password})
        self.headers["Cookie"] = reply.getheader("Set-Cookie").split(";", 1)[0]
        self.headers["X-CSRF-Token"] = data["csrf_token"]
        self.headers["If-Control-Epoch"] = self.get("control")["control_epoch"]

    def call(self, method, path, body=None, extra=None, status=200, browser=True):
        headers = (self.headers if browser else {}) | (extra or {})
        with closing(HTTPSConnection(self.address.hostname, self.address.port, context=self.tls, timeout=45)) as con:
            con.request(method, "/v1/" + path, json.dumps(body) if body is not None else None, headers)
            reply = con.getresponse()
            payload = reply.read()
        assert reply.status in (status if isinstance(status, tuple) else (status,)), (
            method, path, reply.status, payload[:300],
        )
        return reply, json.loads(payload) if payload else {}

    def get(self, path):
        return self.call("GET", path)[1]

    def submit(self, method, path, body=None, *, revision=None, key=None, status=202):
        headers = {"Idempotency-Key": key or uuid4().hex}
        if revision is not None:
            headers["If-Match"] = f'"{revision}"'
        return self.call(method, path, body, headers, status)[1]

    def wait(self, accepted):
        op_id = accepted["operation_id"]
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            op = self.get("operations/" + op_id)
            if op["state"] == "succeeded":
                return op["result"]
            if op["state"] in ("failed", "cancelled", "recovery_required"):
                raise AssertionError(op)
            time.sleep(.2)
        raise TimeoutError(op_id)

    def machine_change(self, machine, suffix, body=None, method="POST"):
        path = "machines/" + machine
        revision = self.get(path)["revision"]
        return self.wait(self.submit(method, path + suffix, body, revision=revision))

    def history(self, machine):
        return {
            op["operation_id"] for op in self.get("operations?limit=500")["items"]
            if op.get("machine_id") == machine
        }


def exercise_readiness(platform, machine, service, token):
    resource = "services/" + service
    details = platform.get(resource)
    initial_revision = details["revision"]
    url = "https://" + details["public_host"]
    wrong = {"readiness": {"type": "http", "path": "/", "expected_status": 204}}
    right = {"readiness": {"type": "http", "path": "/", "expected_status": 200}}
    history = platform.history(machine)
    conflict = platform.submit("PATCH", resource, wrong, revision=initial_revision + 10, status=409)
    assert conflict["error"]["code"] == "REVISION_CONFLICT"
    assert platform.history(machine) == history
    key = uuid4().hex
    accepted = platform.submit("PATCH", resource, wrong, revision=initial_revision, key=key)
    platform.wait(accepted)
    same = platform.submit("PATCH", resource, wrong, revision=initial_revision, key=key)
    assert same["operation_id"] == accepted["operation_id"]
    mismatch = platform.submit("PATCH", resource, right, revision=initial_revision, key=key, status=409)
    assert mismatch["error"]["code"] == "IDEMPOTENCY_CONFLICT"
    updated = platform.get(resource)
    assert updated["revision"] == initial_revision + 1 and updated["readiness"] == wrong["readiness"]

    def request():
        with closing(HTTPSConnection(urlsplit(url).hostname, context=platform.tls, timeout=15)) as connection:
            connection.request("GET", "/", headers={"Authorization": "Bearer " + token})
            reply = connection.getresponse()
            return reply.status, reply.read(), reply.getheader("X-Sandbox-Forwarding")

    status, body, forwarding = request()
    assert status == 409 and json.loads(body)["error"]["code"] == "SERVICE_NOT_READY", (status, body)
    assert forwarding == "not_started"
    with concurrent.futures.ThreadPoolExecutor(2) as ex:
        proposals = list(ex.map(lambda _: platform.submit(
            "PATCH", resource, right, revision=updated["revision"], status=(202, 409)
        ), range(2)))
    proposals_accepted = [item for item in proposals if "operation_id" in item]
    assert len(proposals_accepted) == 1, proposals
    platform.wait(proposals_accepted[0])
    status, body, _ = request()
    assert status == 200 and body == b"Sandbox workspace ready\n", (status, body)
    print("Readiness PATCH enforced revision, replay, concurrency, and actual HTTP probe", flush=True)

    revision = platform.get(resource)["revision"]
    stale = platform.submit("DELETE", resource, {}, revision=initial_revision, status=409)
    assert stale["error"]["code"] == "REVISION_CONFLICT"
    key = uuid4().hex
    deleted = platform.submit("DELETE", resource, {}, revision=revision, key=key)
    platform.wait(deleted)
    platform.call("GET", resource, status=404)
    replay = platform.submit("DELETE", resource, {}, revision=revision, key=key)
    assert replay["operation_id"] == deleted["operation_id"]
    platform.submit("DELETE", resource, {}, revision=revision, status=404)
    print("DELETE replay returned the original operation after physical service removal", flush=True)


def certificate_request(platform, port, certificate, private_key, allowed):
    ctx = ssl.create_default_context(cafile=platform.tls_ca)
    ctx.minimum_version = ssl.TLSVersion.TLSv1_2
    ctx.maximum_version = ssl.TLSVersion.TLSv1_2
    ctx.load_cert_chain(certificate, private_key)
    with socket.create_connection((platform.address.hostname, port), timeout=6) as sock:
        with ctx.wrap_socket(sock, server_hostname=platform.address.hostname) as conn:
            assert conn.version() == "TLSv1.2"
            try:
                conn.sendall(b"GET / HTTP/1.1\r\nHost: local\r\nConnection: close\r\n\r\n")
                body = conn.recv(4096)
            except ConnectionResetError:
                body = b""
            if allowed:
                while b"Sandbox workspace ready\n" not in body and len(body) < 8192:
                    fragment = conn.recv(4096)
                    if not fragment:
                        break
                    body += fragment
                assert b"HTTP/1.1 200 OK" in body and b"Sandbox workspace ready\n" in body, body
            else:
                assert body == b"", body


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--origin", required=True)
    parser.add_argument("--ca", required=True)
    parser.add_argument("--password-file", type=Path, required=True)
    parser.add_argument("--client-csr", type=Path, required=True)
    parser.add_argument("--client-key", required=True)
    parser.add_argument("--template", required=True)
    args = parser.parse_args()
    platform = Platform(args.origin, args.ca, "owner", args.password_file.read_text().strip())
    platform.tls_ca = args.ca

    def create_machine():
        return platform.wait(platform.submit("POST", "machines", {
            "name": "mutation-" + uuid4().hex[:10], "template_id": args.template, "start": True,
            "resources": {"cpu_count": 1, "memory_mib": 512, "storage_gib": 4, "overlay_gib": 2},
            "idle_policy": {"enabled": False, "timeout_seconds": 1800},
        }))["machine_id"]

    left, right = create_machine(), create_machine()
    body = {"label": "object-scoped credential", "scope": {"access": "manage", "machine_id": left}}
    key = uuid4().hex
    token = platform.submit("POST", "tokens", body, key=key, status=200)
    assert token["revision"] == 1 and token["token"]
    repeated = platform.submit("POST", "tokens", body, key=key, status=200)
    assert repeated["token_id"] == token["token_id"] and repeated["revision"] == 1 and "token" not in repeated
    assert next(item for item in platform.get("tokens")["items"] if item["token_id"] == token["token_id"])["revision"] == 1

    fixture = (Path(__file__).resolve().parents[2] / "examples" / "service.pl").read_text()
    tasks = []
    for port in (8080, 8081):
        program = fixture.replace("LocalPort => 8080,", f"LocalPort => {port},")
        tasks.append(platform.machine_change(left, "/tasks", {"argv": ["/usr/bin/perl", "-e", program]})["task_id"])

    http = platform.machine_change(left, "/services", {"name": "demo", "protocol": "http", "guest_port": 8080})["service_id"]
    left_tls, right_tls = [
        platform.machine_change(machine, "/services", {
            "name": "demo-tls", "protocol": "tcp-mtls", "guest_port": 8081,
        })["service_id"] for machine in (left, right)
    ]
    cert = platform.submit("POST", "client-certificates", {
        "csr": args.client_csr.read_text(), "scope": {"service_ids": [left_tls]}, "ttl_seconds": 600,
    }, status=200)
    assert cert["revision"] == 1
    ports = [platform.get("services/" + svc)["public_port"] for svc in (left_tls, right_tls)]
    platform.machine_change(right, "/pause")
    before = platform.history(right)
    err = platform.call("GET", "services/" + right_tls + "/tunnel", extra={
        "Authorization": "Bearer " + token["token"], "Connection": "Upgrade", "Upgrade": "websocket",
    }, status=403, browser=False)[1]
    assert err["error"]["code"] == "PERMISSION_DENIED"
    with tempfile.TemporaryDirectory(prefix="mutation-ingress-") as temp:
        pem = Path(temp) / "client.pem"
        pem.write_text(cert["certificate_pem"])
        certificate_request(platform, ports[1], pem, args.client_key, allowed=False)
        certificate_request(platform, ports[0], pem, args.client_key, allowed=True)
    assert platform.get("machines/" + right)["observed_state"] == "paused"
    assert platform.history(right) == before
    print("Cross-machine token and cross-service certificate were rejected before wake", flush=True)

    exercise_readiness(platform, left, http, token["token"])
    for collection, cid, field in (
        ("tokens", token["token_id"], "token_id"), ("client-certificates", cert["certificate_id"], "id")
    ):
        resource = collection + "/" + cid
        platform.submit("DELETE", resource, status=400)
        reject = platform.submit("DELETE", resource, revision=2, status=409)
        assert reject["error"]["code"] == "REVISION_CONFLICT"
        key = uuid4().hex
        revoked = platform.submit("DELETE", resource, revision=1, key=key, status=200)
        assert revoked == {field: cid, "status": "revoked", "revision": 2}
        assert platform.submit("DELETE", resource, revision=1, key=key, status=200) == revoked
        stale = platform.submit("DELETE", resource, revision=1, status=409)
        assert stale["error"]["code"] == "REVISION_CONFLICT"
        assert next(item for item in platform.get(collection)["items"] if item[field] == cid)["revision"] == 2
    platform.call("GET", "machines", extra={"Authorization": "Bearer " + token["token"]}, status=401, browser=False)
    print("Credential issue/list/revoke revisions and original-key replay passed", flush=True)

    for task in tasks:
        revision = platform.get("machines/" + left)["revision"]
        platform.wait(platform.submit("POST", "tasks/" + task + "/cancel", {"grace_seconds": 0}, revision=revision))
    for machine in (left, right):
        platform.machine_change(machine, "", {}, "DELETE")
    print("Deleted both local mutation test workspaces", flush=True)


if __name__ == "__main__":
    main()
