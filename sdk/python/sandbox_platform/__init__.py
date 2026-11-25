"""Authenticated client for the Sandbox Platform management plane."""

from __future__ import annotations

import base64
import hashlib
import json
from dataclasses import dataclass
from pathlib import Path
from time import monotonic, sleep
from typing import Any, Callable, Iterator
from urllib.parse import quote, unquote, urlsplit, urlunsplit
from uuid import uuid4

import httpx

TERMINAL_OPERATIONS = frozenset(("succeeded", "failed", "cancelled", "recovery_required"))


class PlatformError(Exception):
    def __init__(self, response: httpx.Response):
        self.status = response.status_code
        self.detail = {"message": f"HTTP {self.status}"}
        try:
            detail = response.json()["error"]
            if isinstance(detail, dict):
                self.detail = detail
        except (KeyError, ValueError, TypeError):
            pass
        self.code = self.detail.get("code")
        self.operation_id = self.detail.get("operation_id")
        super().__init__(self.detail.get("message", str(self.detail)))


@dataclass(frozen=True)
class Mutation:
    epoch: str
    key: str


@dataclass(frozen=True)
class Event:
    id: str
    type: str
    data: Any


def _item(collection: str, id: str) -> str:
    return f"{collection}/{quote(id, safe='')}"


class Client:
    def __init__(self, url: str, token: str, *, timeout: float = 90, verify=True):
        origin = urlsplit(url)
        if (
            origin.scheme not in {"http", "https"} or not origin.netloc
            or origin.path not in {"", "/"} or origin.query or origin.fragment
            or origin.username is not None
        ):
            raise ValueError("expected an HTTP or HTTPS platform origin")
        self._url = origin
        self._token = token
        self._http = httpx.Client(
            base_url=url.rstrip("/") + "/v1/",
            headers={"Authorization": f"Bearer {token}"},
            follow_redirects=False,
            timeout=timeout,
            verify=verify,
        )

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()

    def close(self):
        self._http.close()

    @staticmethod
    def _relative(path):
        pathname = path.split("?", 1)[0]
        if (
            not pathname or pathname.startswith("/") or ":" in pathname or "\\" in pathname
            or any(unquote(part) in (".", "..") for part in pathname.split("/"))
        ):
            raise ValueError("expected a relative API path")
        return path

    @staticmethod
    def _check(response: httpx.Response):
        if not response.is_success:
            raise PlatformError(response)
        return response

    def _request(self, method, path, **args):
        return self._check(self._http.request(method, self._relative(path), **args))

    def get(self, path, **params):
        return self._request("GET", path, params=params or None).json()

    def intent(self) -> Mutation:
        return Mutation(self.get("control")["control_epoch"], str(uuid4()))

    def _headers(self, intent, revision):
        intent = intent or self.intent()
        headers = {"If-Control-Epoch": intent.epoch, "Idempotency-Key": intent.key}
        if revision is not None:
            headers["If-Match"] = f'"{revision}"'
        return headers

    def mutate(self, path, body=None, *, method="POST", intent=None, revision=None):
        headers = self._headers(intent, revision)
        res = self._request(method, path, json={} if body is None else body, headers=headers)
        return res.json() if res.status_code != 204 else None

    def upload(self, path, source, *, method="PUT", intent=None, revision=None, digest=False):
        """Send a local file as a bounded-length binary request."""
        headers = self._headers(intent, revision)
        with Path(source).open("rb") as file:
            headers["Content-Type"] = "application/octet-stream"
            headers["Content-Length"] = str(file.seek(0, 2))
            file.seek(0)
            if digest:
                hasher = hashlib.sha256()
                for block in iter(lambda: file.read(65536), b""):
                    hasher.update(block)
                headers["X-Content-SHA256"] = hasher.hexdigest()
                file.seek(0)
            response = self._request(
                method, path, content=iter(lambda: file.read(65536), b""), headers=headers
            )
        return response.json() if response.status_code != 204 else None

    def download(self, path, **params) -> Iterator[bytes]:
        """Yield raw bytes. Closing the iterator releases the response."""
        with self._http.stream("GET", self._relative(path), params=params) as response:
            if not response.is_success:
                response.read()
                self._check(response)
            yield from response.iter_bytes()

    def wait_operation(self, operation_id, timeout=180, *, on_update: Callable | None = None):
        deadline = monotonic() + timeout
        path = _item("operations", operation_id)
        while True:
            remaining = deadline - monotonic()
            if remaining <= 0:
                raise TimeoutError(f"timed out observing operation {operation_id}")
            try:
                op = self._request("GET", path, timeout=min(remaining, 10)).json()
            except httpx.TimeoutException as exc:
                if monotonic() >= deadline:
                    raise TimeoutError(f"timed out observing operation {operation_id}") from exc
                raise
            if on_update is not None:
                on_update(op)
            if op["state"] in TERMINAL_OPERATIONS:
                return op
            sleep(min(0.35, max(0.0, deadline - monotonic())))

    def wait(self, operation_id, timeout=180):
        return self.wait_operation(operation_id, timeout)

    def pages(self, path, *, limit=100, **filters) -> Iterator[dict]:
        cursor = ""
        while True:
            page = self.get(path, limit=limit, cursor=cursor, **filters)
            yield from page["items"]
            nxt = page.get("next_cursor")
            if not nxt:
                return
            if nxt == cursor:
                raise ValueError("server did not advance cursor")
            cursor = nxt

    def task_output(self, task_id, stream="stdout", offset=0):
        """Yield (start, bytes, next, gap). Resume a broken stream with the last next offset."""
        path = _item("tasks", task_id) + "/output"
        while True:
            chunk = self.get(path, stream=stream, offset=offset, limit=65536)
            data = base64.b64decode(chunk["data_base64"] or "", validate=True)
            nxt = chunk["next"]
            if nxt < offset or (data and nxt == offset):
                raise ValueError("invalid task output cursor")
            if data or chunk.get("gap") or nxt != offset:
                yield (chunk["start"], data, nxt, chunk.get("gap", False))
            offset = nxt
            if chunk["complete"]:
                return
            if not data:
                sleep(0.4)

    def events(self, last_event_id="") -> Iterator[Event]:
        """Stream events. Resume after the last ID and resync state on EVENT_CURSOR_EXPIRED."""
        headers = {"Accept": "text/event-stream"}
        if last_event_id:
            headers["Last-Event-ID"] = last_event_id
        with self._http.stream("GET", "events", headers=headers) as response:
            if not response.is_success:
                response.read()
                self._check(response)
            event_id, event_type, data = last_event_id, "message", []
            for line in response.iter_lines():
                if not line:
                    if data:
                        yield Event(event_id, event_type, json.loads("\n".join(data)))
                    event_type, data = "message", []
                    continue
                field, _, value = line.partition(":")
                if value.startswith(" "):
                    value = value[1:]
                if field == "id" and "\0" not in value:
                    event_id = value
                elif field == "event":
                    event_type = value
                elif field == "data":
                    data.append(value)

    def tunnel(self, path, *, protocol="sandbox.tunnel.v1", **options):
        """Connect an authenticated WebSocket with the optional websocket dependency."""
        from websockets.sync.client import connect

        rel = urlsplit(self._relative(path))
        url = urlunsplit(
            ("wss" if self._url.scheme == "https" else "ws", self._url.netloc, "/v1/" + rel.path, rel.query, "")
        )
        return connect(
            url, subprotocols=[protocol], additional_headers={"Authorization": f"Bearer {self._token}"},
            **options
        )

    def create_machine(self, spec, **options):
        """Create from a template. System templates require runtime_instance_id and start=True."""
        return self.mutate("machines", spec, **options)

    def get_machine(self, id):
        return self.get(_item("machines", id))

    def list_volumes(self, *, limit=100):
        return self.pages("volumes", limit=limit)

    def create_volume(self, spec, **options):
        return self.mutate("volumes", spec, **options)

    def get_volume(self, volume_id):
        return self.get(_item("volumes", volume_id))

    def grow_volume(self, volume_id, capacity_bytes, *, intent=None, revision=None):
        if revision is None:
            revision = self.get_volume(volume_id)["revision"]
        return self.mutate(_item("volumes", volume_id), {"capacity_bytes": capacity_bytes},
                           method="PATCH", revision=revision, intent=intent)

    def delete_volume(self, volume_id, *, intent=None, revision=None):
        if revision is None:
            revision = self.get_volume(volume_id)["revision"]
        return self.mutate(_item("volumes", volume_id), method="DELETE", revision=revision, intent=intent)

    def list_volume_snapshots(self, volume_id, *, limit=100):
        return self.pages(_item("volumes", volume_id) + "/snapshots", limit=limit)

    def create_volume_snapshot(self, volume_id, *, intent=None, revision=None):
        if revision is None:
            revision = self.get_volume(volume_id)["revision"]
        return self.mutate(_item("volumes", volume_id) + "/snapshots", revision=revision, intent=intent)

    def get_volume_snapshot(self, snapshot_id):
        return self.get(_item("volume-snapshots", snapshot_id))

    def delete_volume_snapshot(self, snapshot_id, *, intent=None, revision=None):
        if revision is None:
            revision = self.get_volume_snapshot(snapshot_id)["volume_revision"]
        return self.mutate(_item("volume-snapshots", snapshot_id), method="DELETE", revision=revision, intent=intent)

    def clone_volume_snapshot(self, snapshot_id, name, **options):
        return self.mutate(_item("volume-snapshots", snapshot_id) + "/clones", {"name": name}, **options)

    def list_volume_attachments(self, machine_id, *, limit=100):
        return self.pages(_item("machines", machine_id) + "/volume-attachments", limit=limit)

    def attach_volume(self, machine_id, spec, **options):
        body = dict(spec)
        if "expected_volume_revision" not in body:
            body["expected_volume_revision"] = self.get_volume(body["volume_id"])["revision"]
        return self._machine_mutation(machine_id, "volume-attachments", body, **options)

    def detach_volume(self, attachment_id, *, generation, revision, volume_revision, intent=None):
        return self.mutate(
            _item("volume-attachments", attachment_id),
            {"expected_generation": generation, "expected_volume_revision": volume_revision},
            method="DELETE", revision=revision, intent=intent,
        )

    def list_runtime_profiles(self, *, limit=100):
        return self.pages("runtime-profiles", limit=limit)

    def machine_action(self, id, action, *, intent=None, **fields):
        if action not in {"start", "stop", "pause", "resume", "wake"}:
            raise ValueError("unknown machine action")
        return self._machine_mutation(id, action, fields, intent=intent)

    def _machine_mutation(self, id, suffix, body, *, intent=None, revision=None, generation=None):
        if revision is None or generation is None:
            current = self.get_machine(id)
            revision = current["revision"] if revision is None else revision
            generation = current["binding_generation"] if generation is None else generation
        return self.mutate(
            _item("machines", id) + "/" + suffix,
            {**body, "expected_generation": generation}, revision=revision, intent=intent
        )

    def start_task(self, machine_id, spec, **options):
        return self._machine_mutation(machine_id, "tasks", spec, **options)

    def get_task(self, task_id):
        return self.get(_item("tasks", task_id))

    def cancel_task(self, task_id, grace_seconds=5, *, intent=None, revision=None):
        if revision is None:
            revision = self.get_machine(self.get_task(task_id)["machine_id"])["revision"]
        return self.mutate(_item("tasks", task_id) + "/cancel", {"grace_seconds": grace_seconds}, revision=revision, intent=intent)

    def capture(self, machine_id, purpose="clone", label="", *, capture_mode=None, **options):
        body = {"purpose": purpose, "label": label}
        if capture_mode is not None:
            body["capture_mode"] = capture_mode
        return self._machine_mutation(machine_id, "snapshots", body, **options)

    def checkpoint(self, machine_id, label="", **options):
        return self._machine_mutation(machine_id, "snapshots", {"label": label, "capture_mode": "vm"}, **options)

    def fork(self, machine_id, child_name, *, require_independent=False, **options):
        body = {"child_name": child_name}
        if require_independent:
            body["require_independent"] = True
        return self._machine_mutation(machine_id, "forks", body, **options)

    def fork_batch(self, machine_id, children, *, require_independent=True, volume_policy=None, **options):
        """Submit a single capture and keep the ordered child IDs through partial completion."""
        body = {"children": children, "require_independent": require_independent}
        if volume_policy is not None:
            body["volume_policy"] = volume_policy
        return self._machine_mutation(machine_id, "fork-batches", body, **options)

    def standby(self, machine_id, *, activity_policy=None, **options):
        body = {} if activity_policy is None else {"activity_policy": activity_policy}
        return self._machine_mutation(machine_id, "standby", body, **options)

    def hibernate(self, machine_id, *, activity_policy=None, **options):
        body = {} if activity_policy is None else {"activity_policy": activity_policy}
        return self._machine_mutation(machine_id, "hibernate", body, **options)

    def clone(self, machine_id, child_name, **options):
        return self._machine_mutation(machine_id, "clones", {"child_name": child_name, "copy_policy": "managed_clean"}, **options)

    def restore(self, snapshot_id, name, *, volume_restore_policy=None, **options):
        body = {"snapshot_id": snapshot_id, "name": name}
        if volume_restore_policy is not None:
            body["volume_restore_policy"] = volume_restore_policy
        return self.mutate("restores", body, **options)

    def rollback_vm(self, machine_id, snapshot_id, *, volume_restore_policy="clone_conflicts", **options):
        return self._machine_mutation(machine_id, "rollback", {
            "snapshot_id": snapshot_id, "volume_restore_policy": volume_restore_policy,
        }, **options)

    def list_tokens(self):
        return self.pages("tokens")

    def issue_token(self, spec, *, intent=None):
        return self.mutate("tokens", spec, intent=intent)

    def revoke_token(self, token_id, revision, *, intent=None):
        return self.mutate(_item("tokens", token_id), method="DELETE", revision=revision, intent=intent)

    def expose(self, machine_id, service, *, intent=None, revision=None):
        if revision is None:
            revision = self.get_machine(machine_id)["revision"]
        return self.mutate(_item("machines", machine_id) + "/services", service, intent=intent, revision=revision)
