"""Run M1 against a private local serve and the real platformd binary."""
import argparse
from contextlib import ExitStack
from dispatch_proxy import DispatchProxy
import hashlib
import http.client
import json
import pathlib
import secrets
import socket
import subprocess
import tempfile
import time
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", default=".build/platformd")
    parser.add_argument("--socket", default="/run/smolvm-phase1/api.sock")
    parser.add_argument("--image", type=pathlib.Path, required=True)
    parser.add_argument("--interrupt-dispatch", action="store_true",
                        help="use a managed image and kill platformd after native Create and task start have executed")
    args = parser.parse_args()
    image = args.image.resolve(strict=True)
    with ExitStack() as stack:
        tmp = stack.enter_context(tempfile.TemporaryDirectory(prefix="platform-m1-"))
        root = pathlib.Path(tmp)
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        address = f"127.0.0.1:{port}"
        origin = f"http://{address}"
        password = secrets.token_urlsafe()
        pw = root / "owner"
        pw.write_text(password)
        pw.chmod(0o600)
        with image.open("rb") as f:
            digest = hashlib.file_digest(f, "sha256").hexdigest()
        template = {"template_id":"m1", "name":"m1", "version":1, "image_digest":"sha256:"+digest,
                    "spec":{"local_image":image.name}, "capabilities":{}}
        runtime_socket = args.socket
        if args.interrupt_dispatch:
            template["guest_release"] = "v1"
            runtime_socket = str(root / "proxy.sock")
            proxy = stack.enter_context(DispatchProxy(runtime_socket, args.socket))
        (root / "templates").write_text(json.dumps([template]))
        config = root / "config.json"
        config.write_text(json.dumps({"state_dir":tmp, "api":{"listen":address,"public_origin":origin},
            "runtime":{"socket":runtime_socket, "local_image_dir":str(image.parent)},
            "lifecycle":{"startup_idle_guard_seconds":1},"templates_file":str(root/"templates")}))
        import os
        def launch():
            proc = subprocess.Popen([args.binary, "-config", str(config)],env={**os.environ,"SANDBOX_OWNER_PASSWORD_FILE":str(pw)})
            for _ in range(150):
                if proc.poll() is not None:
                    raise RuntimeError(f"platformd exited {proc.returncode}")
                try:
                    with socket.create_connection(("127.0.0.1",port), timeout=.1):
                        return proc
                except OSError:
                    time.sleep(.05)
            proc.terminate()
            proc.wait()
            raise TimeoutError("platformd startup")
        headers = {"Content-Type":"application/json","Origin":origin}
        def request(method,path,body=None,extra=None,expect=None):
            conn = http.client.HTTPConnection(address, timeout=20)
            try:
                conn.request(method,"/v1/"+path,None if body is None else json.dumps(body),headers| (extra or {}))
                resp = conn.getresponse()
                payload=resp.read()
                if expect is not None and resp.status != expect:
                    raise AssertionError((method,path,resp.status,payload[:500]))
                return resp, json.loads(payload)
            finally:
                conn.close()
        proc=launch()
        try:
            res,login=request("POST","auth/login",{"username":"owner","password":password},expect=200)
            headers.update({"Cookie":res.getheader("Set-Cookie").split(";")[0],"X-CSRF-Token":login["csrf_token"]})
            _,ctl=request("GET","control",expect=200)
            assert ctl["runtime_version"]=="1.37.1" and ctl["runtime_ready"]
            headers["If-Control-Epoch"]=ctl["control_epoch"]
            newkey=lambda: {"Idempotency-Key":str(uuid.uuid4())}
            key=newkey()
            intent={"name":"workspace-"+uuid.uuid4().hex[:10],"template_id":"m1","start":True}
            if args.interrupt_dispatch:
                with proxy.intercept("create"):
                    _, accepted = request("POST", "machines", intent, key, 202)
                    assert proxy.response_ready.wait(80), "successful upstream create missing"
                    busy=request("GET", "operations/"+accepted["operation_id"],expect=200)[1]
                    assert busy["state"]=="dispatching" and busy["stage"]=="create",busy
                    proc.kill()
                    proc.wait()
                proc=launch()
            else:
                _,accepted=request("POST","machines",intent,key,202)
            mid=accepted["machine_id"]
            def wait(opid):
                end=time.monotonic()+180
                while time.monotonic()<end:
                    _,op=request("GET","operations/"+opid,expect=200)
                    if op["state"]=="succeeded":
                        print(op["kind"],opid,op["state"],flush=True)
                        return op
                    if op["state"] in ("failed","cancelled","recovery_required"):
                        raise AssertionError(op)
                    time.sleep(.15)
                raise TimeoutError(opid)
            try:
                wait(accepted["operation_id"])
                proc.kill()
                proc.wait()
                proc=launch()
                _,again=request("POST","machines",intent,key,202)
                assert again["operation_id"]==accepted["operation_id"]
                request("POST","machines",intent|{"name":intent["name"]+"-other"},key,409)
                _,machines=request("GET","machines",expect=200)
                assert [m["machine_id"] for m in machines["items"]]==[mid]
                def mutate(kind,method="POST"):
                    _,m=request("GET","machines/"+mid,expect=200)
                    _,op=request(method,"machines/"+mid+("/"+kind if kind else ""),{"expected_generation":1},
                         newkey()|{"If-Match":f'"{m["revision"]}"'},202)
                    return wait(op["operation_id"])
                if args.interrupt_dispatch:
                    assert proxy.count["create"] == 1
                    current = request("GET", "machines/"+mid,expect=200)[1]
                    task_headers = newkey() | {"If-Match": f'"{current["revision"]}"'}
                    task_body = {"argv":["/bin/sh","-c","echo dispatched >> /workspace/dispatch-count; cat /workspace/dispatch-count"],"env":{"GATE_PRIVATE":"LOCAL-ENV-CHECK-71394"},"task_key":"dispatch-proof"}
                    with proxy.intercept("task_start"):
                        _, accepted_task = request("POST","machines/"+mid+"/tasks",task_body,task_headers,202)
                        assert proxy.response_ready.wait(70), "successful guest start missing"
                        proc.kill()
                        proc.wait()
                    proc=launch()
                    result=wait(accepted_task["operation_id"])
                    tid=result["result"]["task_id"]
                    again=request("POST","machines/"+mid+"/tasks",task_body,task_headers,202)[1]
                    assert again["operation_id"]==accepted_task["operation_id"] and proxy.count["task_start"]==1
                    status=request("GET","tasks/"+tid,expect=200)[1]
                    import base64
                    output=request("GET","tasks/"+tid+"/output",expect=200)[1]
                    assert status["state"]=="succeeded" and base64.b64decode(output["data_base64"])==b"dispatched\n"
                    for dbfile in root.glob("platform.db*"):
                        assert b"LOCAL-ENV-CHECK-71394" not in dbfile.read_bytes()
                    print("single native create and guest task dispatch across two SIGKILLs",tid,flush=True)
                    def patch_idle(enabled):
                        cur=request("GET","machines/"+mid,expect=200)[1]
                        change=request("PATCH","machines/"+mid,{"idle_policy":{"enabled":enabled,"timeout_seconds":3}},
                                       newkey()|{"If-Match":f'"{cur["revision"]}"'},202)[1]
                        wait(change["operation_id"])
                    def wait_for(check, seconds=45):
                        end=time.monotonic()+seconds
                        while time.monotonic()<end:
                            val=check()
                            if val:return val
                            time.sleep(.06)
                        raise TimeoutError("activity outcome")
                    cur=request("GET","machines/"+mid,expect=200)[1]
                    saved_activity=cur["last_user_activity_at"]
                    lease=request("POST","machines/"+mid+"/leases",{"ttl_seconds":1,"reason":"idle gate"},
                                  newkey()|{"If-Match":f'"{cur["revision"]}"'},200)[1]
                    patch_idle(True)
                    def expired_lease():
                        items=request("GET","machines/"+mid+"/leases",expect=200)[1]["items"]
                        return next((item for item in items if item["state"]=="suspect"),None)
                    suspect=wait_for(expired_lease)
                    metrics=request("GET","machines/"+mid+"/metrics",expect=200)[1]
                    assert metrics["platform"]["active_access_holds"]==0 and metrics["platform"]["wake_waiters"]==0
                    cur=request("GET","machines/"+mid,expect=200)[1]
                    assert cur["observed_state"]=="running" and cur["last_user_activity_at"]==saved_activity
                    with proxy.intercept("pause"):
                        request("DELETE","leases/"+lease["lease_id"],{"lease_token":lease["lease_token"]},
                                newkey()|{"If-Match":f'"{suspect["revision"]}"'},200)
                        assert proxy.response_ready.wait(45), "native idle pause missing"
                        idle_op=request("GET","machines/"+mid,expect=200)[1]["operation_id"]
                        demand=http.client.HTTPConnection(address)
                        demand.request("GET","/v1/machines/"+mid+"/files/list",headers=headers)
                        try:
                            import sqlite3
                            with sqlite3.connect(f"file:{root}/platform.db?mode=ro",uri=True) as reader:
                                wait_for(lambda: reader.execute("SELECT wake_pending FROM operations WHERE id=?", (idle_op,)).fetchone()[0])
                        finally:
                            demand.close()
                    wait(idle_op)
                    def woke():
                        operations=request("GET","operations",expect=200)[1]["items"]
                        wakes=[op for op in operations if op["kind"]=="wake" and op["machine_id"]==mid]
                        assert len(wakes)<=1
                        return wakes and wakes[0]["state"]=="succeeded"
                    wait_for(woke)
                    patch_idle(False)
                    print("idle capture with a disconnected waiter completed one durable wake", idle_op, flush=True)
                for act in ("pause","resume","stop","start"):
                    mutate(act)
                if args.interrupt_dispatch:
                    cur=request("GET","machines/"+mid,expect=200)[1]
                    task=request("POST","machines/"+mid+"/tasks",{"argv":["/bin/sleep","300"]},
                                 newkey()|{"If-Match":f'"{cur["revision"]}"'},202)[1]
                    task_id=wait(task["operation_id"])["result"]["task_id"]
                    cur=request("GET","machines/"+mid,expect=200)[1]
                    stopped=request("POST","machines/"+mid+"/stop",{"cancel_tasks":True},
                                    newkey()|{"If-Match":f'"{cur["revision"]}"'},202)[1]
                    wait(stopped["operation_id"])
                    assert request("GET","tasks/"+task_id,expect=200)[1]["state"]=="cancelled"
                mutate("","DELETE")
                if args.interrupt_dispatch:
                    proc.terminate()
                    proc.wait()
                    cfg=json.loads(config.read_text())
                    cfg["storage"]={"minimum_free_bytes":1<<60}
                    config.write_text(json.dumps(cfg))
                    proc=launch()
                    low=request("POST","machines",intent|{"name":intent["name"]+"-disk"},newkey(),202)[1]
                    def declined():
                        op=request("GET","operations/"+low["operation_id"],expect=200)[1]
                        return op if op["state"]=="failed" else None
                    outcome=wait_for(declined)
                    assert outcome["effect"]=="none" and outcome["error"]["code"]=="DISK_LOW" and proxy.count["create"]==1
                    print("suspect lease, read-only metrics, and low-space admission verified",flush=True)
            except Exception:
                # Preserve the disposable registry and the exact identities for diagnosis.
                import shutil
                evidence=pathlib.Path(tempfile.mkdtemp(prefix="platform-m1-diagnostic-"))
                proc.terminate()
                proc.wait()
                for f in root.glob("platform.db*"):
                    shutil.copy2(f,evidence/f.name)
                print("diagnostic directory",evidence,"machine",mid,flush=True)
                raise
            print("passed",mid,"control_epoch",ctl["control_epoch"],"archive_sha256",digest)
        finally:
            if proc.poll() is None:
                proc.terminate()
                proc.wait(timeout=20)

if __name__=="__main__":
    main()
