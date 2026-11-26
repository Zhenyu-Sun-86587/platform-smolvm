"""Observe the same real best-effort Fork with compatibility and strict CLI exit codes."""

import json


def check_partial_batch(cli, parent):
    cli.idle_access(parent)
    accepted = cli.json("--no-wait", "machines", "fork-batch", parent, "J11 admitted", "J11 over budget",
                        "--admission", "best_effort")
    operation = accepted["operation_id"]
    observed = cli.json("operations", "wait", operation)
    assert (observed["state"], observed["effect"]) == ("succeeded", "partial"), observed
    strict = cli.run("--json", "--strict-result", "operations", "wait", operation, expected=1)
    terminal = json.loads(strict.stdout)
    assert terminal == observed and not strict.stderr, (terminal, strict.stderr)
    retained, refused = observed["children"]
    assert retained["state"] == "ready" and retained["resource_independence"] == "verified", retained
    assert refused["state"] == "quarantined" and refused["error_code"] == "RESOURCE_LIMIT", refused
    assert observed["result"]["admitted_child_ids"] == [retained["machine_id"]], observed
    # Admission refusal did not create an execution eligible for native repair.
    denied = cli.run("--json", "operations", "retry", operation, expected=1)
    failure = json.loads(denied.stderr)["error"]
    assert failure["effect"] == "none" and not denied.stdout, failure
    unchanged = cli.json("operations", "get", operation)
    assert unchanged["children"] == observed["children"]
    deleted = cli.delete(retained["machine_id"])
    return dict(operation_id=operation, capture_id=observed["result"]["capture_id"],
                children=[retained["machine_id"], refused["machine_id"]], refusal=refused["error_code"],
                retry_error=failure["code"], compatibility_exit=0, strict_exit=1, delete_operation_id=deleted)
