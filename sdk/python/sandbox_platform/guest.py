"""Guest-scoped control and credential proxy requests over Linux virtio-vsock."""

from contextlib import contextmanager
from http.client import HTTPConnection
import json
import socket
from urllib.parse import urlsplit
from uuid import uuid4

from . import Mutation, _item


class GuestError(Exception):
    def __init__(self, status, detail):
        self.status = status
        self.code = detail.get("code")
        self.operation_id = detail.get("operation_id")
        super().__init__(detail.get("message", f"Guest relay HTTP {status}"))


class _GuestConnection(HTTPConnection):
    def connect(self):
        channel = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
        try:
            channel.settimeout(self.timeout)
            channel.connect((socket.VMADDR_CID_HOST, self.port))
        except BaseException:
            channel.close()
            raise
        self.sock = channel


class GuestClient:
    """Use the installation's Guest credential on the hypervisor's relay port."""

    def __init__(self, token: str, *, port: int = 6001, timeout: float = 90):
        self._token = token
        self._port = port
        self._timeout = timeout

    @contextmanager
    def _send(self, method, path, body=None, headers=None):
        connection = _GuestConnection("guest-relay", port=self._port, timeout=self._timeout)
        try:
            connection.request(method, "/v1/" + path, body=body, headers={
                "Authorization": f"Bearer {self._token}", "Connection": "close", **(headers or {}),
            })
            with connection.getresponse() as response:
                yield response
        finally:
            connection.close()

    def _json(self, method, path, body=None, headers=None):
        if body is not None:
            body = json.dumps(body).encode()
            headers = {"Content-Type": "application/json", **(headers or {})}
        with self._send(method, path, body, headers) as response:
            raw = response.read()
            if not 200 <= response.status < 300:
                try:
                    detail = json.loads(raw)["error"]
                except (ValueError, TypeError, KeyError):
                    detail = {}
                raise GuestError(response.status, detail if isinstance(detail, dict) else {})
            return json.loads(raw)

    def intent(self):
        return Mutation(self._json("GET", "control")["control_epoch"], str(uuid4()))

    def _mutate(self, path, body, intent=None, revision=None):
        intent = intent or self.intent()
        headers = {"If-Control-Epoch": intent.epoch, "Idempotency-Key": intent.key}
        if revision is not None:
            headers["If-Match"] = f'"{revision}"'
        return self._json("POST", path, body, headers)

    def create(self, name, *, intent=None):
        return self._mutate("machines", {"name": name}, intent)

    def fork(self, machine_id, name, *, revision, generation, intent=None):
        return self._mutate(_item("machines", machine_id) + "/forks", {
            "child_name": name, "expected_generation": generation,
        }, intent, revision)

    def machine(self, machine_id):
        return self._json("GET", _item("machines", machine_id))

    def operation(self, operation_id):
        return self._json("GET", _item("operations", operation_id))

    def exec(self, machine_id, command, *, timeout_seconds=60, cwd=None, env=None, stdin=None):
        body = {"command": command, "timeout_seconds": timeout_seconds}
        body.update({key: value for key, value in {"cwd": cwd, "env": env, "stdin": stdin}.items() if value is not None})
        return self._json("POST", _item("machines", machine_id) + "/exec", body)

    def proxy_stream(self, proxy_id, path, *, method="GET", body=None, content_type=None, accept=None):
        """Open a bounded upstream response. Read or iterate it inside the context."""
        route = urlsplit(path)
        if (route.scheme or route.netloc or route.fragment or not route.path.startswith("/")
                or any(part in ("", ".", "..") for part in route.path[1:].split("/"))
                or any(char in route.path for char in "%\\")):
            raise ValueError("expected an allowlisted absolute upstream path")
        headers = {}
        if content_type:
            headers["Content-Type"] = content_type
        if accept:
            headers["Accept"] = accept
        return self._send(method, _item("credential-proxies", proxy_id) + path, body, headers)
