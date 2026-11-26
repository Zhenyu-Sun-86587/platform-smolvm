"""Real stock OpenSSH/SFTP and private machine HTTPS through a disposable System Guest."""

from contextlib import ExitStack, closing
import hashlib
import json
import os
import select
from pathlib import Path
import subprocess
import time
from urllib.parse import urlencode, urlsplit

from authorized_services_kvm import PROGRAM, SERVICE_PORT, machine, write_service
from managed_system_vsock import wait_for
from service_revision_kvm import mutate


def check(platform, args, keys):
    mid, view, ref = machine(platform, "ssh-first-use", args.mode)
    path = "machines/" + mid
    issued = platform.change("tokens", "managed-ssh-token", {
        "label": "Disposable first login", "scope": {"access": "manage", "machine_id": mid},
    }, status=200)
    credential = issued["token"]
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
        assert run(args.sandboxctl, "ssh", "ssh-first-use", "--", "id -u") == b"0\n"
        assert run("ssh", "-tt", "-oBatchMode=yes", alias,
                   "test -t 0 && test -t 1 && printf 'pty ready\\n'; exit 24", code=24) == b"pty ready\r\n"
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
        preview_status, payload, _ = web(authority, "POST", "/receipt/preview-cookie", {"Cookie": cookie})
        assert (preview_status, payload) == (200, b"preview-cookie\n"), (preview_status, payload)
        assert web(original, "POST", "/receipt/wrong-origin", {"Cookie": cookie})[0] == 403
        receipts = platform.guest(ref, ["cat", "/run/authorized-service-receipts"]).splitlines()
        assert receipts == [b"editor-forward", b"machine-web", b"original-web", b"preview-cookie"], receipts
        # A fresh SSH connection also follows the normal wake gate.
        parked = platform.wait_capability(path, "lifecycle.standby")
        platform.complete(platform.change(path + "/standby", "managed-ssh-standby", {}, parked))
        assert platform.app(path)["power_state"] == "standby"
        assert run(args.sandboxctl, "ssh", mid, "--", "id -u") == b"0\n"
        assert platform.app(path)["power_state"] == "running"
        live = platform.wait_capability(path, "state.hibernate")
        platform.complete(platform.change(path + "/hibernate", "managed-ssh-hibernate", {}, live))
        assert platform.app(path)["power_state"] == "hibernated"
        assert run(args.sandboxctl, "ssh", mid, "--", "id -u") == b"0\n"
        assert platform.context(mid)[1]["execution_epoch"] > ref["execution_epoch"]
        # The local key stays on disk, but revocation must sever this live transport.
        with subprocess.Popen(["ssh", "-oBatchMode=yes", alias, "echo connected; exec sleep 90"],
                              env=env, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                              stderr=subprocess.DEVNULL) as active:
            try:
                assert select.select([active.stdout], [], [], 45)[0] and active.stdout.readline() == b"connected\n"
                tick = time.monotonic()
                receipt = mutate(platform, "DELETE", "tokens/" + issued["token_id"], "revoke-managed-ssh",
                                 issued["revision"], {}, 200)
                assert receipt["status"] == "revoked"
                assert active.wait(timeout=12) == 255
                revoke_ms = round(1000 * (time.monotonic() - tick))
            finally:
                if active.poll() is None:
                    active.kill()
        run("ssh", "-oBatchMode=yes", alias, "id -u", code=255)
        print(json.dumps(dict(case="managed-ssh-and-web", mode=args.mode, machine_id=mid, transport="worker_guest_agent",
                              new_user_exit=23, agent=False, published_ssh=False,
                              revocation_ms=revoke_ms, sftp_bytes=len(blob), sftp_sha256=hashlib.sha256(blob).hexdigest(),
                              direct_tcpip=True, pty_exit=24, web_receipts=len(receipts), standby_wake=True, hibernate_wake=True)), flush=True)


def check_networkless(platform, args, keys):
    mid, _, ref = machine(platform, "isolated-ssh", light=True)
    path = "machines/" + mid
    view = platform.app(path)
    assert not view.get("network"), view.get("network")
    token = platform.change("tokens", "isolated-manage", {
        "label": "Isolated machine", "scope": {"access": "manage", "machine_id": mid},
    }, status=200)["token"]
    env = {**os.environ, "SANDBOX_URL": platform.origin, "SANDBOX_TOKEN": token,
           "SANDBOX_CA_FILE": str(keys / "ca.pem"), "SSH_ASKPASS_REQUIRE": "never"}
    env.pop("SSH_AUTH_SOCK", None)
    # Force a large discrepancy between Guest and host realtime in this disposable
    # VM. SSH must authenticate normally and preserve the Guest's chosen clock.
    platform.guest(ref, ["date", "-u", "-s", "@1600000000"])
    response = subprocess.run([args.sandboxctl, "ssh", "--batch-mode", mid, "--", "id -u; date -u +%s"],
                              env=env, capture_output=True, timeout=90)
    assert response.returncode == 0, response.stderr.replace(token.encode(), b"<redacted>")
    uid, timestamp = response.stdout.decode().split()
    assert uid == "0" and 1600000000 <= int(timestamp) < 1600000600, (uid, timestamp)
    assert platform.app(path + "/services")["items"] == []
    print(json.dumps(dict(case="managed-ssh-networkless", machine_id=mid, guest_clock=int(timestamp),
                          network=False, published_services=0, ssh_uid=uid)), flush=True)
