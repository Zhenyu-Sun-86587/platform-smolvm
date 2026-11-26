#!/usr/bin/env python3
"""Run the production console against a dedicated System guest and private VNC service."""
import argparse
from contextlib import ExitStack
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

from managed_system_vsock import Platform, wait_for


def run(args, root, stack):
    network = dict(network_id="console-net", route_domain_id="console-test", cidr="10.64.42.0/24")
    platform = Platform(args, root, stack, (network,), state_tmpfs=True, use_installation=True)
    password = os.urandom(24).hex()
    platform.start(owner_password=password)
    catalog_path = root / "templates.json"
    templates = json.loads(catalog_path.read_text())
    source = templates[0]
    templates.append({**source, "template_id": "t_console_desktop", "name": "Console desktop",
                      "spec": {**source["spec"], "desktop_profile": "software-vnc-v1"}})
    catalog_path.write_text(json.dumps(templates))
    platform.restart()

    def browser(scenario, *arguments):
        # Pass the ephemeral owner password through stdin. Logs contain only assertions and receipts.
        child = subprocess.run([args.node, Path(__file__).with_name("console-machine.mjs"), scenario,
                                platform.origin, *arguments], input=password, text=True,
                               stdout=subprocess.PIPE, timeout=240, env={
                                   **os.environ,
                                   **({"CONSOLE_DIST": str(args.web_dist)} if args.web_dist else {}),
                                   "CHROMIUM": str(args.chromium), "PLAYWRIGHT_CORE": str(args.playwright_core),
                                   **({"CONSOLE_EVIDENCE": str(args.evidence_dir)} if args.evidence_dir else {}),
                                   **({"LD_LIBRARY_PATH": str(args.chromium_library_dir)} if args.chromium_library_dir else {}),
                               })
        if child.stdout:
            print(child.stdout.rstrip(), flush=True)
        child.check_returncode()
        return json.loads(child.stdout.splitlines()[-1])

    created = browser("create", platform.serve_id, network["network_id"])
    machine = created["machine_id"]
    accepted = platform.complete(dict(operation_id=created["operation_id"]))
    assert accepted["machine_id"] == machine and accepted["state"] == "succeeded"
    view, execution = platform.context(machine)
    wait_for(lambda: platform.guest(execution, ["/bin/cat", "/proc/1/comm"]) == b"systemd\n",
             retry=(OSError, RuntimeError))
    failed = []
    for suite in ("guest", "observe"):
        if args.suite in (suite, "all"):
            try:
                browser(suite, machine)
            except subprocess.CalledProcessError:
                if args.suite != "all":
                    raise
                failed.append(suite)
    if args.suite in ("guest", "observe"):
        assert platform.context(machine)[1] == execution
        print(json.dumps(dict(bundle=platform.bundle_id, execution_epoch=execution["execution_epoch"],
                              suite=args.suite)), flush=True)
        return

    # The Debian desktop image includes Xvfb and x11vnc. Use the private network in this namespace.
    start = ("set -e\nXvfb :5 -screen 0 800x480x24 -nolisten tcp &\n"
             "for i in $(seq 1 60); do test ! -S /tmp/.X11-unix/X5 || break; sleep .1; done\n"
             "exec x11vnc -display :5 -rfbport 5900 -forever -shared -nopw")
    platform.guest(execution, ["/usr/bin/systemd-run", "--unit=console-vnc", "/bin/sh", "-c", start])
    wait_for(lambda: platform.guest(execution, ["/bin/sh", "-c", "ss -ltn | grep -q ':5900 '"]) == b"",
             retry=(OSError, RuntimeError, AssertionError), timeout=25)
    source = "machines/" + machine
    service_op = platform.change(source + "/services", "create-console-vnc",
                                 dict(name="Test desktop", protocol="vnc", guest_port=5900),
                                 machine=platform.app(source))
    service = platform.complete(service_op)["result"]["service_id"]
    target = platform.app("services/" + service)
    assert target["protocol"] == "vnc" and not target.get("public_port")
    browser("desktop", machine)
    assert platform.context(machine)[1] == execution
    print(json.dumps(dict(bundle=platform.bundle_id, binding_generation=view["binding_generation"],
                          execution_epoch=execution["execution_epoch"], console="verified")), flush=True)
    if failed:
        raise RuntimeError("Browser suites failed: " + ", ".join(failed))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for flag in ("--platformd", "--bundle", "--system-image", "--node", "--chromium", "--playwright-core"):
        parser.add_argument(flag, type=lambda value: Path(value).resolve(strict=True), required=True)
    parser.add_argument("--web-dist", type=lambda value: Path(value).resolve(strict=True),
                        help="Override the embedded UI with local production assets at the same origin")
    parser.add_argument("--work-dir", type=Path, default=Path("/var/tmp"))
    parser.add_argument("--chromium-library-dir", type=Path)
    parser.add_argument("--evidence-dir", type=lambda value: Path(value).resolve(strict=True),
                        help="Existing directory for screenshots from the disposable guest")
    parser.add_argument("--suite", choices=["guest", "desktop", "observe", "all"], default="all",
                        help="The desktop check requires an image with Xvfb and x11vnc installed")
    parser.add_argument("--isolated", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if not args.isolated:
        subprocess.run(["unshare", "--mount", "--propagation", "private", "--net", "--",
                        sys.executable, "-B", __file__, *sys.argv[1:], "--isolated"], check=True)
        return
    with tempfile.TemporaryDirectory(dir=args.work_dir, prefix="console-kvm-") as folder, ExitStack() as stack:
        root = Path(folder)
        root.chmod(0o755)
        run(args, root, stack)


if __name__ == "__main__":
    main()
