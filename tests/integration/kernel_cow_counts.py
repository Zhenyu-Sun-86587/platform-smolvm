"""Count Native materialization in the measured source VMM on a disposable x86 Worker."""

import ctypes
import errno
import os
from pathlib import Path
import re
import struct
import subprocess


# Match only the defining Rust symbol, excluding generic wrappers and closures.
SYMBOLS = {
    "capture_boundary": "16capture_boundary",
    "native_export": "15export_resource",
    "native_delta_copy": "15copy_into_delta",
}


def backing_blocks(proc):
    backings = {}
    for fd in (proc / "fd").iterdir():
        try:
            if os.readlink(fd) == "/memfd:smolvm-guest-ram (deleted)":
                stat = fd.stat()
                backings[(stat.st_dev, stat.st_ino)] = stat.st_blocks * 512
        except FileNotFoundError:
            continue
    return backings


class CaptureCounters:
    def __init__(self, platform, execution, stack):
        self.pid = execution["pid"]
        self.proc = Path("/proc") / str(self.pid)
        self.start = execution["pid_start_time"]
        self.verify_source()
        lib = (platform.bundle / "lib/libkrun.so.2").resolve(strict=True)
        st = lib.stat()
        maps = []
        for line in (self.proc / "maps").read_text().splitlines():
            span, mode, offset, dev, inode, *_ = line.split(maxsplit=5)
            if int(inode) == st.st_ino and os.makedev(*(int(x, 16) for x in dev.split(":"))) == st.st_dev:
                lo, hi = (int(x, 16) for x in span.split("-"))
                maps.append((lo, hi, mode, int(offset, 16)))
        base, = [lo for lo, _, _, offset in maps if offset == 0]
        nm = subprocess.check_output(["nm", "--defined-only", lib], text=True)
        addresses = {}
        for name, suffix in SYMBOLS.items():
            value, = re.findall(r"^([0-9a-fA-F]+) [tT] _RNv\w*8krun_vmm\w*" + suffix + "$", nm, re.M)
            address = base + int(value, 16)
            assert any(lo <= address < hi and "x" in mode for lo, hi, mode, _ in maps), name
            addresses[name] = address

        shared = platform.root / "capture-counts.so"
        fixture = Path(__file__).resolve().parents[1] / "fixtures/kernel-cow-counters.c"
        subprocess.run(["cc", "-shared", "-fPIC", "-Wall", "-Wextra", "-Werror", fixture, "-o", shared], check=True)
        helper = ctypes.CDLL(str(shared), use_errno=True)
        probe = helper.open_fork_counter
        probe.argtypes = (ctypes.c_int, ctypes.c_size_t)
        probe.restype = ctypes.c_int
        self.fds = {name: [] for name in SYMBOLS}
        for thread in (self.proc / "task").iterdir():
            for name, address in addresses.items():
                fd = probe(int(thread.name), address)
                if fd < 0:
                    code = ctypes.get_errno()
                    if code == errno.ESRCH:
                        break
                    raise OSError(code, f"{name} on TID {thread.name}: {os.strerror(code)}")
                stack.callback(os.close, fd)
                self.fds[name].append(fd)
        self.before = backing_blocks(self.proc)
        self.verify_source()

    def verify_source(self):
        assert int((self.proc / "stat").read_text().rsplit(")", 1)[1].split()[19]) == self.start

    def read(self, backend):
        self.verify_source()
        calls = {}
        for name, descriptors in self.fds.items():
            samples = [struct.unpack("=QQQ", os.read(fd, 24)) for fd in descriptors]
            # Inactive threads may not have been scheduled. A multiplexed or
            # unscheduled capture cannot substantiate a zero.
            assert samples and any(running for _, _, running in samples), name
            assert all(enabled == running for _, enabled, running in samples), (name, samples)
            calls[name] = sum(count for count, _, _ in samples)
        assert calls["capture_boundary"] == 1, calls
        after = backing_blocks(self.proc)
        allocations = [blocks for key, blocks in after.items() if key not in self.before]
        if backend == "kernel_cow":
            assert calls["native_export"] == calls["native_delta_copy"] == 0 and not allocations, (calls, allocations)
        else:
            assert calls["native_export"] == 1 and calls["native_delta_copy"] > 0, calls
            assert sum(allocations) >= 16 << 20, allocations
        self.verify_source()
        return dict(source_pid=self.pid, source_start_ticks=self.start, calls=calls,
                    new_ram_backing_allocated_bytes=sum(allocations), new_ram_backings=len(allocations))
