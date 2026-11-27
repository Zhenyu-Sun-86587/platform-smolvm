"""Check the Python SDK's public memory Fork payloads and results with a fake API."""

import json

import httpx

from sandbox_platform import Client, Mutation, PlatformError


PROFILE = "kernel_cow_anon_v1"
FORK = {
    "operation_id": "op_fork", "kind": "independent_fork", "state": "succeeded", "effect": "applied",
    "result": {"requested_memory_fork_backend": "auto", "selected_memory_fork_backend": "kernel_cow",
               "memory_fork_profile": PROFILE},
}
MEMORY = {"profile": PROFILE, "kernel_cow_supported": True, "native_supported": False}


def main():
    writes = []

    def handle(request):
        path = request.url.path
        if request.method == "POST":
            assert request.headers["if-control-epoch"] == "epoch"
            assert request.headers["idempotency-key"] == "intent"
            body = json.loads(request.content)
            writes.append((path, body))
            if body.get("child_name") == "denied":
                return httpx.Response(409, json={"error": {
                    "code": "CAPABILITY_UNSUPPORTED", "capability": "state.memory_fork",
                    "reason_code": "MEMORY_FORK_BACKEND_UNAVAILABLE", "effect": "none",
                }})
            return httpx.Response(202, json=FORK if path.endswith("/forks") and body.get("memory_fork_backend") == "auto"
                                  else {"operation_id": "op_queued", "state": "queued"})
        if path == "/v1/runtime-profiles":
            return httpx.Response(200, json={"items": [{"capabilities": {"boot.kernel_cow_ram": {
                "status": "supported", "supported": True, "contract_version": 1, "verified_profile": PROFILE,
            }}}]})
        if path == "/v1/machines/m_source":
            return httpx.Response(200, json={"revision": 7, "binding_generation": 3, "memory_fork_profile": PROFILE,
                                             "capabilities": {"state.independent_fork": {"memory_fork": MEMORY}}})
        assert path == "/v1/operations/op_fork", request.url
        return httpx.Response(200, json=FORK)

    intent = Mutation("epoch", "intent")
    with Client("http://localhost", "fixture") as client:
        client._http.close()
        client._http = httpx.Client(base_url="http://localhost/v1/", transport=httpx.MockTransport(handle))
        profile, = client.list_runtime_profiles()
        assert profile["capabilities"]["boot.kernel_cow_ram"]["verified_profile"] == PROFILE
        machine = client.get_machine("m_source")
        assert machine["capabilities"]["state.independent_fork"]["memory_fork"] == MEMORY
        spec = {"name": "source", "template_id": "system", "runtime_instance_id": "worker", "memory_fork_profile": PROFILE}
        client.create_machine(spec, intent=intent)
        client.fork("m_source", "legacy", intent=intent)
        try:
            client.fork("m_source", "malformed", memory_fork_backend="native", intent=intent)
        except ValueError as error:
            assert "require_independent=True" in str(error)
        else:
            raise AssertionError("expected an explicit independent Fork guard")
        receipt = client.fork("m_source", "child", require_independent=True, memory_fork_backend="auto", intent=intent)
        assert receipt == client.wait_operation("op_fork") == FORK
        client.fork_batch("m_source", [{"name": "batch"}], memory_fork_backend="kernel_cow", intent=intent)
        try:
            client.fork("m_source", "denied", require_independent=True, memory_fork_backend="native", intent=intent)
        except PlatformError as error:
            assert (error.capability, error.reason_code, error.effect) == (
                "state.memory_fork", "MEMORY_FORK_BACKEND_UNAVAILABLE", "none",
            )
        else:
            raise AssertionError("expected the server's backend rejection")

    assert writes == [
        ("/v1/machines", spec),
        ("/v1/machines/m_source/forks", {"child_name": "legacy", "expected_generation": 3}),
        ("/v1/machines/m_source/forks", {"child_name": "child", "expected_generation": 3,
                                        "require_independent": True, "memory_fork_backend": "auto"}),
        ("/v1/machines/m_source/fork-batches", {"children": [{"name": "batch"}], "expected_generation": 3,
                                               "require_independent": True, "memory_fork_backend": "kernel_cow"}),
        ("/v1/machines/m_source/forks", {"child_name": "denied", "expected_generation": 3,
                                        "require_independent": True, "memory_fork_backend": "native"}),
    ]
    print("Python SDK memory Fork contract passed")


if __name__ == "__main__":
    main()
