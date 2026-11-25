# Run a workspace workflow

Use an installed platform with a registered managed template. Set `SANDBOX_URL` to the management origin, `SANDBOX_TOKEN` to a management token, `SANDBOX_TEMPLATE` to the managed template ID, and `SANDBOX_NAME` to a new workspace name. Use a different name for each run.

The examples create a started workspace, accept a persistent task, reconnect to its output by byte cursor, read its exit status, then capture a clean snapshot and clone the workspace. They print operation IDs and leave the workspaces and snapshots available for inspection.

```sh
python -m pip install ./sdk/python
python examples/agent_workflow.py
```

```sh
npm ci --prefix sdk/typescript
npm run build --prefix sdk/typescript
node examples/agent-workflow.mjs
```

Set `SANDBOX_EXAMPLE_SERVICE=1` to also run an HTTP task in the clone and request it through the stable HTTPS service endpoint. This requires wildcard DNS and TLS for the configured apps domain and Python in the Guest template. The example cancels the HTTP task when the check ends.

For CLI use, set the same `SANDBOX_URL` and `SANDBOX_TOKEN`.

```sh
sandboxctl templates list
sandboxctl machines create agent-demo --template "$SANDBOX_TEMPLATE"
sandboxctl machines list
sandboxctl tasks start MACHINE_ID -- /bin/sh -c 'echo first; sleep 5; echo last'
sandboxctl tasks logs TASK_ID --follow
sandboxctl snapshots capture MACHINE_ID --purpose clone
```

An operation wait timeout can be recovered by resuming observation of the original operation ID with `sandboxctl operations wait OPERATION_ID`. When replaying a mutation after a lost response, pass its original `--control-epoch`, `--idempotency-key`, and applicable `--revision` and `--generation`.
