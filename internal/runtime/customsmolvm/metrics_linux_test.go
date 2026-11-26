package customsmolvm

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	rt "sandbox-platform/internal/runtime"
)

func TestCgroupMetricsDistinguishZeroFromMissing(t *testing.T) {
	values := readMachineCounters(fstest.MapFS{
		"cpu.stat":       {Data: []byte("usage_usec 0\nuser_usec 0\n")},
		"memory.current": {Data: []byte("bad\n")},
		"cpu.max":        {Data: []byte("75000 100000\n")},
		"memory.max":     {Data: []byte("max\n")},
	})
	if values.cpuMillis == nil || *values.cpuMillis != 0 || values.memoryMB != nil ||
		values.cpuLimit == nil || *values.cpuLimit != 750 || values.memoryLimit != nil {
		t.Fatalf("unexpected metrics: %+v", values)
	}
	if values.issues[metricMemoryMB].Code != "CGROUP_READ_FAILED" || values.issues[metricMemoryLimitMB].Code != "LIMIT_UNBOUNDED" || len(values.issues) != 2 {
		t.Fatalf("unexpected missing fields: %+v", values.issues)
	}
}

func TestCgroupMetricsRequireExactWorkerProcessScope(t *testing.T) {
	root := t.TempDir()
	proc, cgroups := filepath.Join(root, "proc"), filepath.Join(root, "cgroup")
	write := func(path, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	const serve, name = "70716575656e6f74626567756e6a7573", "machine-a"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	group := "dedicated_1.slice/smolvmdeep-vm-" + serve + "-" + hash[:32] + ".scope"
	write(filepath.Join(proc, "sys/kernel/random/boot_id"), "boot-a\n")
	write(filepath.Join(proc, "12/stat"), "12 (vm ) name) S "+strings.Repeat("0 ", 18)+"32 0\n")
	write(filepath.Join(proc, "12/cgroup"), "0::/"+group+"\n")
	write(filepath.Join(cgroups, group, "cpu.stat"), "usage_usec 1241234\n")
	write(filepath.Join(cgroups, group, "memory.current"), "33554432\n")
	var inventory rt.NativeInventory
	inventory.Execution.InstanceID = "i-" + serve + "-" + hash
	inventory.Execution.HostBootID = "boot-a"
	inventory.Execution.PID, inventory.Execution.PIDStartTime = 12, 32
	sample := newMachineMetrics(name)
	if err := observeMachineScope(&sample, proc, cgroups, serve, name, inventory); err != nil {
		t.Fatal(err)
	}
	if sample.Availability != "partial" || sample.ObservedAt == nil || sample.CPUMillis == nil || *sample.CPUMillis != 1241 ||
		sample.MemoryMB == nil || *sample.MemoryMB != 32 || sample.RSSMB != nil {
		t.Fatalf("unexpected identified sample: %+v", sample)
	}
	for _, wrong := range []string{"sibling", "wrong_boot", "reused_pid"} {
		t.Run(wrong, func(t *testing.T) {
			verified := inventory
			scopeName := name
			switch wrong {
			case "sibling":
				scopeName = "another-machine"
			case "wrong_boot":
				verified.Execution.HostBootID = "boot-b"
			case "reused_pid":
				verified.Execution.PIDStartTime++
			}
			sample := newMachineMetrics(name)
			if err := observeMachineScope(&sample, proc, cgroups, serve, scopeName, verified); err == nil ||
				sample.ObservedAt != nil || sample.CPUMillis != nil || sample.MemoryMB != nil {
				t.Fatalf("unverified process yielded a sample: %+v, %v", sample, err)
			}
		})
	}
}
