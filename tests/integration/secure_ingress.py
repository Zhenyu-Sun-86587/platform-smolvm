"""Real OpenSSH via WebSocket, mTLS half-close/revocation and automatic idle."""
import argparse
import base64
import hashlib
import json
import os
import pathlib
import ssl
import subprocess
import tempfile
import time
import uuid
from http.client import HTTPSConnection


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port",type=int,required=True)
    parser.add_argument("--password-file",type=pathlib.Path,required=True)
    parser.add_argument("--test-pki",type=pathlib.Path,required=True)
    parser.add_argument("--template",default="debian-managed-v1")
    args=parser.parse_args()
    origin=f"https://127.0.0.1:{args.port}"
    ca=args.test_pki/"ca.pem"
    tls=ssl.create_default_context(cafile=str(ca))
    headers={"Origin":origin,"Content-Type":"application/json"}
    def call(method,path,body=None,hdr=None,raw=False):
        conn=HTTPSConnection("127.0.0.1",args.port,context=tls,timeout=100)
        try:
            conn.request(method,"/v1/"+path,(body if raw else json.dumps(body).encode()) if body is not None else None,headers|(hdr or {}))
            res=conn.getresponse();data=res.read()
            if not 200<=res.status<300:raise RuntimeError((method,path,res.status,data[:300]))
            return res,json.loads(data) if data else {}
        finally:conn.close()
    def get(path):return call("GET",path)[1]
    def wait(check,timeout=60):
        end=time.monotonic()+timeout
        while time.monotonic()<end:
            value=check()
            if value:return value
            time.sleep(.25)
        raise TimeoutError(check)
    def change(path,body,mid=None,method="POST",revision=None):
        extra={"Idempotency-Key":uuid.uuid4().hex}
        if mid:revision=get("machines/"+mid)["revision"]
        if revision:extra["If-Match"]=f'"{revision}"'
        doc=call(method,path,body,extra)[1]
        if not doc.get("operation_id"):return doc
        oid=doc["operation_id"]
        def result():
            op=get("operations/"+oid)
            if op["state"] in ("failed","cancelled","recovery_required"):raise AssertionError(op)
            return op["result"] if op["state"]=="succeeded" else None
        return wait(result)
    res,login=call("POST","auth/login",{"username":"owner","password":args.password_file.read_text().strip()})
    headers.update({"Cookie":res.getheader("Set-Cookie").split(";")[0],"X-CSRF-Token":login["csrf_token"]})
    headers["If-Control-Epoch"]=get("control")["control_epoch"]
    with tempfile.TemporaryDirectory(prefix="ingress-") as tmp:
        stage=pathlib.Path(tmp);key=stage/"identity"
        subprocess.run(["ssh-keygen","-qt","ed25519","-N","","-f",str(key)],check=True)
        pk=change("ssh-keys",{"label":"ingress fixture","public_key":key.with_suffix(".pub").read_text()})
        mid=change("machines",{"name":"ingress-"+uuid.uuid4().hex[:8],"template_id":args.template,"start":True,"idle_policy":{"enabled":False}})["machine_id"]
        base="machines/"+mid
        ssh=change(base+"/services",{"name":"ssh","protocol":"ssh","guest_port":22},mid)["service_id"]
        cred=change("tokens",{"label":"ssh gate","scope":{"access":"tunnel","machine_id":mid}})
        pub=get("services/"+ssh).get("ssh_host_key")
        if not pub:raise AssertionError("missing verified SSH host public key")
        known=stage/"known_hosts";known.write_text("sandbox-"+mid+" "+pub+"\n")
        proxy=stage/"sandboxctl"
        subprocess.run(["go","build","-o",str(proxy),"./cmd/sandboxctl"],check=True)
        ssh_cmd=["ssh","-F","/dev/null","-i",str(key),"-o","IdentitiesOnly=yes","-o","StrictHostKeyChecking=yes","-o",f"UserKnownHostsFile={known}","-o","BatchMode=yes","-o",f"HostKeyAlias=sandbox-{mid}","-o",f"ProxyCommand={proxy} ssh-proxy --machine {mid}","root@workspace"]
        env=os.environ|{"SANDBOX_TOKEN":cred["token"],"SANDBOX_CA_FILE":str(ca),"SANDBOX_URL":origin}
        uid=subprocess.check_output(ssh_cmd+["id -u"],env=env,timeout=30).strip()
        assert uid==b"0",uid
        binary=stage/"counter"
        subprocess.run(["go","build","-o",str(binary),"./tests/fixtures/counter"],env=os.environ|{"CGO_ENABLED":"0"},check=True)
        data=binary.read_bytes()
        call("PUT",base+"/files?path=/workspace/counter",data,{"Idempotency-Key":uuid.uuid4().hex,"If-Match":f'"{get(base)["revision"]}"',"Content-Type":"application/octet-stream","X-Content-SHA256":hashlib.sha256(data).hexdigest()},True)
        task=change(base+"/tasks",{"argv":["/bin/sh","-c","chmod 700 /workspace/counter && exec /workspace/counter"]},mid)["task_id"]
        svc=change(base+"/services",{"name":"echo","protocol":"tcp-mtls","guest_port":8081},mid)["service_id"]
        port=get("services/"+svc)["public_port"]
        cert=change("client-certificates",{"csr":(args.test_pki/"client.csr").read_text(),"scope":{"service_ids":[svc]},"ttl_seconds":1200})
        certfile=stage/"client.pem";certfile.write_text(cert["certificate_pem"])
        client=stage/"halfclose";subprocess.run(["go","build","-o",str(client),"./tests/fixtures/halfclose"],check=True)
        cmd=[str(client),f"127.0.0.1:{port}",str(ca),str(certfile),str(args.test_pki/"client.key")]
        change(base+"/pause",{"allow_active_tasks":True},mid)
        # Connecting without a client cert cannot start a wake operation.
        import socket
        try:
            with socket.create_connection(("127.0.0.1",port),timeout=4) as sock:
                with tls.wrap_socket(sock,server_hostname="127.0.0.1") as c:c.sendall(b"probe");c.recv(20)
        except (ssl.SSLError,OSError):pass
        assert get(base)["observed_state"]=="paused"
        subprocess.run(cmd,check=True,timeout=50)
        change("client-certificates/"+cert["certificate_id"],{},method="DELETE",revision=1)
        change(base+"/pause",{"allow_active_tasks":True},mid)
        assert subprocess.run(cmd,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=8).returncode!=0
        assert get(base)["observed_state"]=="paused"
        # The same stable SSH destination wakes the saved execution and retains its verified host key.
        assert subprocess.check_output(ssh_cmd+["id -u"],env=env,timeout=40).strip()==b"0"
        change(base,{"idle_policy":{"enabled":True,"timeout_seconds":2}},mid,"PATCH")
        time.sleep(6)
        assert get(base)["observed_state"]=="running" and get("tasks/"+task)["state"]=="running"
        change("tasks/"+task+"/cancel",{"grace_seconds":0},mid)
        wait(lambda:get(base)["observed_state"]=="paused",40)
        change(base+"/resume",{},mid)
        change(base,{"idle_policy":{"enabled":False}},mid,"PATCH")
        change(base,{},mid,"DELETE")
        change("ssh-keys/"+pk["id"],{},method="DELETE",revision=1)
        print("verified SSH UID 0, stable host identity, mTLS TLS half-close and pre-wake certificate revocation, task hold and idle pause",mid,flush=True)

if __name__=="__main__":main()
