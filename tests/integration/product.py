"""Exercise task reconnect, live copy, clean checkpoint restore, stable HTTP and teardown."""
import argparse
import json
import pathlib
import subprocess
import time
import uuid
from http.client import HTTPConnection

def main():
    parser=argparse.ArgumentParser()
    parser.add_argument("--port",type=int,required=True)
    parser.add_argument("--password-file",type=pathlib.Path,required=True)
    parser.add_argument("--template",default="debian-managed-v1")
    parser.add_argument("--apps-domain",default="apps.example.test")
    parser.add_argument("--through",choices=["task","fork","clone","gateway"],default="gateway")
    parser.add_argument("--gateway-only",action="store_true")
    args=parser.parse_args()
    addr=f"127.0.0.1:{args.port}"
    headers={"Origin":"http://"+addr,"Content-Type":"application/json"}
    def api(method,path,body=None,extra=None):
        conn=HTTPConnection(addr,timeout=120)
        payload=json.dumps(body) if body is not None else None
        try:
            conn.request(method,"/v1/"+path,payload,headers|(extra or {}))
            res=conn.getresponse()
            raw=res.read()
            if not 200<=res.status<300:
                raise RuntimeError((method,path,res.status,raw[:600]))
            return res,json.loads(raw) if raw else None
        finally:
            conn.close()
    def get(path):
        return api("GET",path)[1]
    def post(path,body,mid=None,method="POST",revision=None):
        hdr={"Idempotency-Key":uuid.uuid4().hex}
        if mid:revision=get("machines/"+mid)["revision"]
        if revision:hdr["If-Match"]=f'"{revision}"'
        doc=api(method,path,body,hdr)[1]
        if "operation_id" not in doc:return doc
        oid=doc["operation_id"]
        deadline=time.monotonic()+300
        while time.monotonic()<deadline:
            op=get("operations/"+oid)
            if op["state"]=="succeeded":
                print(path,oid,op["state"],flush=True)
                return op
            if op["state"] in ("failed","recovery_required","cancelled"):
                raise AssertionError(op)
            time.sleep(.2)
        raise TimeoutError(oid)
    res,login=api("POST","auth/login",{"username":"owner","password":args.password_file.read_text().strip()})
    headers["Cookie"]=res.getheader("Set-Cookie").split(";")[0]
    headers["X-CSRF-Token"]=login["csrf_token"]
    headers["If-Control-Epoch"]=get("control")["control_epoch"]
    testid=uuid.uuid4().hex[:10]
    parent=post("machines",{"name":"product-"+testid,"template_id":args.template,"start":True})["machine_id"]
    base="machines/"+parent
    if args.gateway_only:
        clone=parent
        clone_base=base
    else:
        task=post(base+"/tasks",{"argv":["/bin/sh","-c","printf 'first\\n'; sleep 2; printf 'second\\n'; exit 7"],"task_key":"reconnect-"+testid},parent)["result"]["task_id"]
        deadline=time.monotonic()+30
        while time.monotonic()<deadline:
            snap=get("tasks/"+task)
            if snap["state"]=="failed":break
            time.sleep(.25)
        assert snap["exit_code"]==7,snap
        import base64
        out=get("tasks/"+task+"/output?offset=0&limit=3&stream=stdout")
        data=base64.b64decode(out["data_base64"])
        nxt=get(f'tasks/{task}/output?offset={out["next"]}&stream=stdout')
        assert data+base64.b64decode(nxt["data_base64"])==b"first\nsecond\n"
        if args.through=="task":
            post(base,{},parent,"DELETE");return
        active=post(base+"/tasks",{"argv":["/bin/sh","-c","while :; do echo tick; sleep 1; done"]},parent)["result"]["task_id"]
        child=post(base+"/forks",{"child_name":"fork-"+testid},parent)["machine_id"]
        cbase="machines/"+child
        copied=get(cbase+"/tasks")["items"]
        assert any(t.get("origin_task_id")==active and t["state"]=="running" for t in copied),copied
        newtask=next(t["id"] for t in copied if t.get("origin_task_id")==active)
        post("tasks/"+newtask+"/cancel",{"grace_seconds":0},child)
        assert get("tasks/"+active)["state"]=="running"
        post(cbase,{},child,"DELETE")
        post("tasks/"+active+"/cancel",{"grace_seconds":0},parent)
        if args.through=="fork":
            post(base,{},parent,"DELETE");return
        snap=post(base+"/snapshots",{"purpose":"clone","label":testid},parent)["result"]["snapshot_id"]
        clone=post("restores",{"snapshot_id":snap,"name":"restore-"+testid})["machine_id"]
        clone_base="machines/"+clone
        assert get(clone_base)["observed_state"]=="running"
    if args.through=="gateway":
        # Upload a static oracle via the platform file API before it is executed in the Guest.
        import tempfile,os,hashlib,concurrent.futures
        with tempfile.TemporaryDirectory() as tmp:
            program=pathlib.Path(tmp)/"counter"
            subprocess.run(["go","build","-o",str(program),"./tests/fixtures/counter"],check=True,env=os.environ|{"CGO_ENABLED":"0"})
            data=program.read_bytes()
        conn=HTTPConnection(addr,timeout=60)
        try:
            _,rev=api("GET",clone_base)
            conn.request("PUT","/v1/"+clone_base+"/files?path=/workspace/counter",data,headers|{"Content-Type":"application/octet-stream","Idempotency-Key":uuid.uuid4().hex,"X-Content-SHA256":hashlib.sha256(data).hexdigest(),"If-Match":f'"{rev["revision"]}"'})
            resp=conn.getresponse();payload=resp.read();assert 200<=resp.status<300,(resp.status,payload[:200])
        finally:conn.close()
        server=post(clone_base+"/tasks",{"argv":["/bin/sh","-c","chmod 700 /workspace/counter && exec /workspace/counter"]},clone)["result"]["task_id"]
        svc=post(clone_base+"/services",{"name":"test-http","protocol":"http","guest_port":8080},clone)["result"]["service_id"]
        token=post("tokens",{"label":"gate","scope":{"access":"tunnel","machine_id":clone}})["token"]
        def http(method,path,auth=token):
            conn=HTTPConnection(addr,timeout=100)
            try:
                conn.request(method,path,headers={"Host":svc+"."+args.apps_domain,"Authorization":"Bearer "+auth,"Idempotency-Key":"test-only-counter-key"})
                res=conn.getresponse();return res.status,res.read(),res.getheader("X-Sandbox-Forwarding")
            finally:conn.close()
        assert http("GET","/")[:2]==(200,b"0")
        post(clone_base+"/pause",{"allow_active_tasks":True},clone)
        assert http("GET","/","invalid")[0]==401 and get(clone_base)["observed_state"]=="paused"
        with concurrent.futures.ThreadPoolExecutor(max_workers=20) as pool:
            results=list(pool.map(lambda _:http("GET","/")[:2],range(20)))
        assert results==[(200,b"0")]*20,results
        wakes=[op for op in get("operations?limit=500")["items"] if op.get("machine_id")==clone and op["kind"]=="wake"]
        assert len(wakes)==1 and wakes[0]["state"]=="succeeded",wakes
        status,_,forwarded=http("POST","/apply-and-drop")
        assert status==502 and forwarded=="may_have_started" and http("GET","/")[:2]==(200,b"1"),(status,forwarded)
        post("tasks/"+server+"/cancel",{"grace_seconds":0},clone)
    post(clone_base,{},clone,"DELETE")
    if clone!=parent:post(base,{},parent,"DELETE")

if __name__=="__main__":main()
