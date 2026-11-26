"""Real stock OpenSSH/SFTP and private machine HTTPS through a disposable System Guest."""

from contextlib import ExitStack, closing
import hashlib
import json
import os
from pathlib import Path
import subprocess
from urllib.parse import urlencode, urlsplit

from authorized_services_kvm import PROGRAM, SERVICE_PORT, machine, write_service
from managed_system_vsock import wait_for


def check(platform, args, keys):
    mid, view, ref = machine(platform, "ssh-first-use", "native")
    path = "machines/" + mid
    credential = platform.change("tokens", "managed-ssh-token", {
        "label": "Disposable first login", "scope": {"access": "manage", "machine_id": mid},
    }, status=200)["token"]
    authority = urlsplit(view["web_url"]).hostname
    assert authority == "vm-" + mid[2:] + ".owned-services.test"
    env = {k: v for k, v in os.environ.items() if not k.startswith(("SANDBOX", "SSH_", "XDG_"))}
    env.update(HOME="/root", SSH_ASKPASS_REQUIRE="never")

    def run(*command, data=b"", code=0):
        p = subprocess.run(command, input=data, capture_output=True, timeout=100, env=env)
        if p.returncode != code:
            msg = p.stderr.decode(errors="replace").replace(credential, "<redacted>")
            raise AssertionError((command, code, p.returncode, msg[-2000:]))
        return p.stdout

    def web(host, method, route, headers=None, body=b""):
        with closing(platform.api_connection(timeout=45)) as connection:
            connection.request(method, route, headers={"Host": host, **(headers or {})}, body=body)
            reply = connection.getresponse()
            return reply.status, reply.read(), reply.getheaders()

    directory = platform.root / "fresh-home"
    directory.mkdir(mode=0o700)
    with ExitStack() as cleanup:
        # OpenSSH resolves the default config using the account's passwd home.
        # The harness has a private mount namespace and discards everything at exit.
        subprocess.run(["mount", "--bind", directory, "/root"], check=True)
        cleanup.callback(subprocess.run, ["umount", "/root"], check=True)
        # Use a stock empty system configuration as well: the development
        # host has cluster-only Include files outside this isolated account.
        system_config = platform.root / "system-ssh-config"
        system_config.write_text("# Disposable OpenSSH client defaults\n")
        subprocess.run(["mount", "--bind", system_config, "/etc/ssh/ssh_config"], check=True)
        cleanup.callback(subprocess.run, ["umount", "/etc/ssh/ssh_config"], check=True)
        assert not Path("/root/.ssh").exists()
        configured = run(args.sandboxctl, "login", "--token-stdin", "--url", platform.origin,
                         "--ca-file", str(keys / "ca.pem"), data=(credential + "\n").encode())
        assert credential.encode() not in configured
        assert credential not in Path("/root/.ssh/config").read_text()
        assert not any(x["protocol"] == "ssh" for x in platform.app(path + "/services")["items"])
        alias = "ssh-first-use.sandbox"
        assert run("ssh", "-oBatchMode=yes", alias, "id -u; exit 23", code=23) == b"0\n"
        connection = run("ssh", "-oBatchMode=yes", alias, 'printf "%s" "$SSH_CONNECTION"').decode().split()
        assert len(connection) == 4 and connection[-1] == "2222", connection
        assert run(args.sandboxctl, "ssh", "ssh-first-use", "--", "id -u") == b"0\n"
        blob = os.urandom(36865)
        source, downloaded = directory / "source.bin", directory / "downloaded.bin"
        source.write_bytes(blob)
        run("scp", "-q", source, alias + ":/tmp/managed-ssh-roundtrip")
        run("scp", "-q", alias + ":/tmp/managed-ssh-roundtrip", downloaded)
        assert downloaded.read_bytes() == blob
        bearer = {"Authorization": "Bearer " + credential}
        assert web(authority, "GET", "/", bearer)[0] == 404
        navigation = {"Accept": "text/html", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}
        status, _, redirect = web(authority, "GET", "/", navigation)
        assert status == 302 and dict(redirect)["Location"] == platform.origin + "/#/machines/" + mid + "/networking?open=web"
        assert web(authority, "POST", "/", navigation)[0] == 401
        write_service(platform, ref)
        platform.guest(ref, ["systemd-run", "--unit=managed-http", "/usr/bin/perl", PROGRAM, "http", str(SERVICE_PORT)])
        wait_for(lambda: f":{SERVICE_PORT:04X}".encode() in platform.guest(ref, ["cat", "/proc/net/tcp"]))
        forwarded = run("ssh", "-oBatchMode=yes", "-W", f"127.0.0.1:{SERVICE_PORT}", alias,
                        data=b"POST /receipt/editor-forward HTTP/1.1\r\nHost: localhost\r\nContent-Length: 0\r\n\r\n")
        assert forwarded.endswith(b"\r\n\r\neditor-forward\n"), forwarded

        sid = platform.complete(platform.change(path + "/services", "managed-web-service", {
            "name": "web", "protocol": "http", "guest_port": SERVICE_PORT,
        }, platform.app(path)))["result"]["service_id"]
        assert web(authority, "POST", "/receipt/denied")[0] == 401
        assert web(authority, "POST", "/receipt/machine-web", bearer)[:2] == (200, b"machine-web\n")
        original = platform.app("services/" + sid)["public_host"]
        assert original != authority
        assert web(original, "POST", "/receipt/original-web", bearer)[:2] == (200, b"original-web\n")
        ticket = platform.app("services/" + sid + "/preview-ticket?address=machine", "POST")
        assert ticket["action"] == "https://" + authority + "/.sandbox/session"
        status, _, headers = web(authority, "POST", "/.sandbox/session",
                                 {"Origin": platform.origin, "Content-Type": "application/x-www-form-urlencoded"},
                                 urlencode({"ticket": ticket["ticket"]}).encode())
        assert status == 303
        cookie = next(v.split(";")[0] for k, v in headers if k.lower() == "set-cookie")
        assert web(authority, "POST", "/receipt/preview-cookie", {"Cookie": cookie})[:2] == (200, b"preview-cookie\n")
        assert web(original, "POST", "/receipt/wrong-origin", {"Cookie": cookie})[0] == 403
        receipts = platform.guest(ref, ["cat", "/run/authorized-service-receipts"]).splitlines()
        assert receipts == [b"editor-forward", b"machine-web", b"original-web", b"preview-cookie"], receipts
        # A fresh SSH connection also follows the normal wake gate.
        parked = platform.wait_capability(path, "lifecycle.standby")
        platform.complete(platform.change(path + "/standby", "managed-ssh-standby", {}, parked))
        assert platform.app(path)["power_state"] == "standby"
        after = run("ssh", "-oBatchMode=yes", alias, 'id -u; printf "%s" "$SSH_CONNECTION"').decode().split()
        assert after[0] == "0" and after[1] == connection[0] and after[-1] == "2222", after
        assert platform.app(path)["power_state"] == "running"
        print(json.dumps(dict(case="managed-ssh-and-web", machine_id=mid, ssh_source=connection[0],
                              guest_port=connection[-1], new_user_exit=23, agent=False, published_ssh=False,
                              sftp_bytes=len(blob), sftp_sha256=hashlib.sha256(blob).hexdigest(),
                              direct_tcpip=True, web_receipts=len(receipts), standby_wake=True)), flush=True)
