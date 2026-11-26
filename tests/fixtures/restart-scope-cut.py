#!/usr/bin/python3
"""Hold one StartTransientUnit call in the private component-restart fixture."""

import json
import os
from pathlib import Path
import subprocess
import sys
import time


def main():
    control = Path(os.environ["A20_SCOPE_CUT"])
    real = str(control / "busctl")
    args = sys.argv[1:]
    try:
        scope, stage = json.loads((control / "armed").read_text())
    except FileNotFoundError:
        scope, stage = None, None
    if len(args) < 9 or args[4] != "StartTransientUnit" or args[6] != scope:
        os.execv(real, [real, *args])
    pid = int(args[args.index("PIDs") + 3])
    start = int(Path(f"/proc/{pid}/stat").read_text().rsplit(")", 1)[1].split()[19])
    if stage == "after":
        subprocess.run([real, *args], check=True)
    marker = control / ("seen-" + stage)
    temporary = marker.with_suffix(".tmp")
    temporary.write_text(json.dumps(dict(pid=pid, start=start, serve=os.getppid())))
    temporary.rename(marker)
    deadline = time.monotonic() + 12
    while time.monotonic() < deadline:
        if (control / ("release-" + stage)).exists():
            return 0 if stage == "after" else 1
        time.sleep(.02)
    return 1


if __name__ == "__main__":
    sys.exit(main())
