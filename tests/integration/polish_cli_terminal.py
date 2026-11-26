"""Check the native sandboxctl PTY and the OpenSSH entry on one disposable System Guest."""

import errno
from contextlib import ExitStack
import fcntl
import json
import os
import pty
import select
import signal
import struct
import subprocess
import termios
import time

from authorized_services_kvm import create_service, machine
from managed_system_vsock import wait_for


def read_until(fd, expected, timeout=40):
    data = bytearray()
    end = time.monotonic() + timeout
    while expected not in data:
        remaining = end - time.monotonic()
        if remaining <= 0 or not select.select([fd], [], [], remaining)[0]:
            raise AssertionError(("PTY output deadline", bytes(data), expected))
        try:
            chunk = os.read(fd, 8192)
        except OSError as error:
            if error.errno != errno.EIO:
                raise
            chunk = b""
        if not chunk:
            raise AssertionError(("PTY closed", bytes(data), expected))
        data.extend(chunk)
    return bytes(data)


def window(slave, rows, cols):
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


def terminal(args, env, machine_id):
    master, slave = pty.openpty()
    try:
        window(slave, 24, 80)
        shell = ('test -t 0 && test -t 1 || exit 99; printf "initial=%s\\n" "$(stty size)"; '
                 'IFS= read -r line; for i in $(seq 1 50); do '
                 'test "$(stty size)" = "31 101" && break; sleep .1; done; '
                 'printf "received=%s,size=%s\\n" "$line" "$(stty size)"; exit 27')
        proc = subprocess.Popen([args.sandboxctl, "exec", "--tty", "--cwd", "/", machine_id,
                                 "--", "/bin/sh", "-c", shell], env=env, stdin=slave,
                                stdout=slave, stderr=slave)
        try:
            read_until(master, b"initial=24 80")
            window(slave, 31, 101)
            proc.send_signal(signal.SIGWINCH)
            os.write(master, b"cli-pty-input\n")
            read_until(master, b"received=cli-pty-input,size=31 101")
            assert proc.wait(timeout=15) == 27, proc.returncode
        finally:
            if proc.poll() is None:
                proc.kill()
            proc.wait()
    finally:
        os.close(master)
        os.close(slave)


def agent_login(platform, args, mid, env, private_key):
    # The fixture already runs as root in a private mount namespace. Hide its
    # default OpenSSH identities so only the disposable agent can authenticate.
    directory = platform.root / "empty-root"
    directory.mkdir(mode=0o700)
    with ExitStack() as stack:
        subprocess.run(["mount", "--bind", directory, "/root"], check=True)
        stack.callback(subprocess.run, ["umount", "/root"], check=True)
        sock = platform.root / "ssh-agent.sock"
        agent = subprocess.Popen(["ssh-agent", "-D", "-a", sock], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        stack.callback(agent.wait, 10)
        stack.callback(agent.terminate)
        wait_for(sock.is_socket, timeout=5)
        env = {**env, "SSH_AUTH_SOCK": str(sock), "SSH_ASKPASS_REQUIRE": "never"}
        assert subprocess.run(["ssh-add", "-l"], env=env, capture_output=True).returncode == 1
        def cli(*words):
            return subprocess.run([args.sandboxctl, *words], env=env, stdin=subprocess.DEVNULL,
                                  capture_output=True, timeout=65)
        native = cli("exec", "--follow", "--cwd", "/", mid, "--", "/usr/bin/id", "-u")
        assert native.returncode == 0 and native.stdout == b"0\n", native.stderr
        denied = cli("ssh", "--legacy", mid, "--batch-mode", "--", "id -u")
        assert denied.returncode == 255 and b"Permission denied" in denied.stderr, (denied.returncode, denied.stderr)
        subprocess.run(["ssh-add", private_key], env=env, check=True, capture_output=True)
        connected = cli("ssh", "--legacy", mid, "--", "id -u; exit 22")
        assert connected.returncode == 22 and connected.stdout == b"0\n", (connected.returncode, connected.stderr)
        print(json.dumps(dict(case="cli-ssh-agent", machine_id=mid, native_exec_without_ssh_key=True,
                              empty_agent_exit=denied.returncode, ssh_login_without_identity_flag=True,
                              guest_exit=connected.returncode)), flush=True)


def check(platform, args, keys, private_key):
    mid, _, ref = machine(platform, "cli-terminal", "native")
    # Manage includes read access to the verified host key and scoped tunnel access.
    credential = platform.change("tokens", "cli-terminal-token", {
        "label": "CLI terminal fixture", "scope": {"access": "manage", "machine_id": mid},
    }, status=200)["token"]
    env = {**os.environ, "SANDBOX_TOKEN": credential, "SANDBOX_URL": platform.origin,
           "SANDBOX_CA_FILE": str(keys / "ca.pem")}
    terminal(args, env, mid)
    sid = create_service(platform, mid, "ssh", 2222)
    wait_for(lambda: platform.app("services/" + sid).get("ssh_host_key"))
    ssh = subprocess.run([args.sandboxctl, "ssh", mid, "--batch-mode", "--identity", private_key,
                          "--user", "root", "--", "id -u; exit 21"], env=env,
                         stdin=subprocess.DEVNULL, capture_output=True, timeout=65)
    assert ssh.returncode == 21 and ssh.stdout == b"0\n", (ssh.returncode, ssh.stdout, ssh.stderr)
    agent_login(platform, args, mid, env, private_key)
    assert platform.context(mid)[1] == ref
    print(json.dumps(dict(case="cli-terminal", machine_id=mid, service_id=sid,
                          bundle=platform.bundle_id, execution_epoch=ref["execution_epoch"],
                          pty_initial=[24, 80], pty_resized=[31, 101], pty_exit=27,
                          ssh_exit=21, ssh_stdout=ssh.stdout.decode())), flush=True)
    from polish_cli_power import check as check_power
    check_power(platform, args, mid, env)
