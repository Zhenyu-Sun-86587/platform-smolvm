package customsmolvm

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
)

const (
	metricCPUMillis      = "cpu_millis"
	metricCPULimitMillis = "cpu_limit_millicpus"
	metricMemoryMB       = "memory_mb"
	metricMemoryLimitMB  = "memory_limit_mb"
)

func unavailableMetric(message string) rt.MetricIssue {
	return rt.MetricIssue{Code: "METRIC_NOT_SAMPLED", Message: message}
}

func newMachineMetrics(name string) rt.Metrics {
	return rt.Metrics{Name: name, State: "unknown", MetricObservation: &rt.MetricObservation{
		Availability: "unavailable", Source: "cgroup_v2", Scope: "machine",
		Issues: map[string]rt.MetricIssue{
			"vcpus":        unavailableMetric("Guest vCPU count is not sampled by the VMM scope"),
			"rss_mb":       unavailableMetric("VMM process RSS is not sampled by this collector"),
			"disk_used_mb": unavailableMetric("Guest disk usage is not sampled by the VMM scope"),
			"disk_gb":      unavailableMetric("Guest disk capacity is not sampled by the VMM scope"),
			"egress_bytes": unavailableMetric("Guest network egress is not sampled by the VMM scope"),
			"pressure":     unavailableMetric("VMM scope pressure is not sampled by this collector"),
		},
	}}
}

func machineMetricsUnavailable(sample rt.Metrics, code, message string) rt.Metrics {
	for _, field := range []string{metricCPUMillis, metricCPULimitMillis, metricMemoryMB, metricMemoryLimitMB} {
		sample.Issues[field] = rt.MetricIssue{Code: code, Message: message}
	}
	return sample
}

func (a *Adapter) Metrics(ctx context.Context, name string) (rt.Metrics, error) {
	ref, err := a.DiscoverExecution(ctx, name)
	if err != nil {
		return machineMetricsUnavailable(newMachineMetrics(name), "EXECUTION_UNAVAILABLE", "there is no verifiable current execution"), nil
	}
	return a.MachineMetrics(ctx, name, ref)
}

// MachineMetrics checks the protected Worker inventory before reading counters.
// Every read targets the host-owned VMM cgroup for the supplied execution.
func (a *Adapter) MachineMetrics(ctx context.Context, name string, ref rt.ExecutionRef) (rt.Metrics, error) {
	sample := newMachineMetrics(name)
	if err := ctx.Err(); err != nil {
		return sample, err
	}
	if !validExecution(ref) {
		return machineMetricsUnavailable(sample, "EXECUTION_UNAVAILABLE", "there is no verifiable current execution"), nil
	}
	sample.Execution = &rt.MetricExecution{IncarnationID: ref.IncarnationID, ExecutionEpoch: ref.ExecutionEpoch}
	inventory, err := a.NativeInventory(ctx, ref)
	if err != nil {
		if ctx.Err() != nil {
			return sample, ctx.Err()
		}
		return machineMetricsUnavailable(sample, "EXECUTION_UNAVAILABLE", "Worker inventory could not verify this execution"), nil
	}
	switch inventory.Inventory.SourceDisposition {
	case "RUNNING":
		sample.State = "running"
	case "STANDBY":
		sample.State = "standby"
	default:
		return machineMetricsUnavailable(sample, "EXECUTION_NOT_SAMPLEABLE", "Worker has not confirmed a running or standby execution"), nil
	}
	if err := observeMachineScope(&sample, "/proc", "/sys/fs/cgroup", a.route.InstanceID, name, inventory); err != nil {
		return machineMetricsUnavailable(sample, "CGROUP_SCOPE_UNAVAILABLE", "the VMM cgroup and its process identity could not be verified"), nil
	}
	return sample, nil
}

func observeMachineScope(sample *rt.Metrics, proc, cgroups, serve, name string, inventory rt.NativeInventory) error {
	vm := inventory.Execution
	bootID, err := os.ReadFile(filepath.Join(proc, "sys/kernel/random/boot_id"))
	if err != nil || vm.HostBootID == "" || strings.TrimSpace(string(bootID)) != vm.HostBootID || vm.PID < 2 || vm.PIDStartTime < 1 {
		return errors.New("Worker PID is outside the local host boot scope")
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
	if vm.InstanceID != "i-"+serve+"-"+hash || !identifier.MatchString(serve) {
		return errors.New("Worker instance identity differs from the metric scope")
	}
	scopeName := "smolvmdeep-vm-" + serve + "-" + hash[:32] + ".scope"
	process := filepath.Join(proc, strconv.Itoa(vm.PID))
	group, err := verifiedProcessScope(process, scopeName, vm.PIDStartTime)
	if err != nil {
		return err
	}
	path := filepath.Join(cgroups, group)
	scope, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer scope.Close()
	identity, err := scope.Stat(".")
	if err != nil {
		return err
	}
	sampling := time.Now().UTC()
	values := readMachineCounters(scope.FS())
	currentGroup, err := verifiedProcessScope(process, scopeName, vm.PIDStartTime)
	if err != nil || currentGroup != group {
		return errors.New("process scope changed during the sample")
	}
	current, err := os.Stat(path)
	if err != nil || !os.SameFile(identity, current) {
		return errors.New("cgroup was replaced during the sample")
	}
	if values.cpuMillis != nil || values.memoryMB != nil || values.cpuLimit != nil || values.memoryLimit != nil {
		sample.Availability = "partial"
		sample.ObservedAt = &sampling
		sample.At = sampling.Format(time.RFC3339Nano)
	}
	sample.CPUMillis, sample.MemoryMB, sample.CPULimitMillis = values.cpuMillis, values.memoryMB, values.cpuLimit
	if values.memoryLimit != nil {
		sample.MemoryLimitMB = *values.memoryLimit
	}
	for field, issue := range values.issues {
		sample.Issues[field] = issue
	}
	return nil
}

func verifiedProcessScope(process, scope string, startTime int64) (string, error) {
	stat, err := os.ReadFile(filepath.Join(process, "stat"))
	if err != nil {
		return "", err
	}
	// The command name can contain spaces or closing parentheses. The kernel
	// terminates it with a final ") " before the state and numeric fields.
	split := strings.LastIndex(string(stat), ") ")
	if split < 0 {
		return "", errors.New("invalid process stat")
	}
	fields := strings.Fields(string(stat[split+2:]))
	if len(fields) <= 19 || fields[0] == "Z" || fields[0] == "X" || fields[19] != strconv.FormatInt(startTime, 10) {
		return "", errors.New("process identity changed")
	}
	groups, err := os.ReadFile(filepath.Join(process, "cgroup"))
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(string(groups)) {
		relative, ok := strings.CutPrefix(strings.TrimSpace(line), "0::/")
		if !ok {
			continue
		}
		slice, leaf, _ := strings.Cut(relative, "/")
		stem, ok := strings.CutSuffix(slice, ".slice")
		if leaf == scope && ok && isDedicatedSlice(stem) {
			return relative, nil
		}
	}
	return "", errors.New("process is outside its managed VMM cgroup")
}

func isDedicatedSlice(stem string) bool {
	if stem == "" || len(stem) > 63 || stem == "system" || stem == "user" || stem == "machine" || stem == "smolvm" {
		return false
	}
	for _, ch := range stem {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_') {
			return false
		}
	}
	return true
}

type machineCounters struct {
	cpuMillis, memoryMB   *uint64
	cpuLimit, memoryLimit *uint32
	issues                map[string]rt.MetricIssue
}

func readMachineCounters(files fs.FS) machineCounters {
	v := machineCounters{issues: make(map[string]rt.MetricIssue)}
	v.cpuMillis = cumulativeCPUMillis(files)
	if v.cpuMillis == nil {
		v.issues[metricCPUMillis] = fieldReadFailed("cgroup cpu.stat did not report cumulative CPU time")
	}
	if data, err := fs.ReadFile(files, "memory.current"); err == nil {
		if bytes, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err == nil {
			mib := bytes >> 20
			v.memoryMB = &mib
		}
	}
	if v.memoryMB == nil {
		v.issues[metricMemoryMB] = fieldReadFailed("cgroup memory.current did not report charged memory")
	}
	v.cpuLimit, v.issues[metricCPULimitMillis] = readCPULimit(files)
	if v.cpuLimit != nil {
		delete(v.issues, metricCPULimitMillis)
	}
	v.memoryLimit, v.issues[metricMemoryLimitMB] = readMemoryLimit(files)
	if v.memoryLimit != nil {
		delete(v.issues, metricMemoryLimitMB)
	}
	return v
}

func fieldReadFailed(message string) rt.MetricIssue {
	return rt.MetricIssue{Code: "CGROUP_READ_FAILED", Message: message}
}

func readMemoryLimit(files fs.FS) (*uint32, rt.MetricIssue) {
	data, err := fs.ReadFile(files, "memory.max")
	if err == nil {
		text := strings.TrimSpace(string(data))
		if text == "max" {
			return nil, rt.MetricIssue{Code: "LIMIT_UNBOUNDED", Message: "VMM scope memory limit is unbounded"}
		}
		if bytes, err := strconv.ParseUint(text, 10, 64); err == nil && bytes>>20 <= math.MaxUint32 {
			mib := uint32(bytes >> 20)
			return &mib, rt.MetricIssue{}
		}
	}
	return nil, fieldReadFailed("cgroup memory.max did not report the VMM scope limit")
}

func cumulativeCPUMillis(files fs.FS) *uint64 {
	data, err := fs.ReadFile(files, "cpu.stat")
	if err != nil {
		return nil
	}
	for line := range strings.Lines(string(data)) {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			if usec, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				millis := usec / 1000
				return &millis
			}
			return nil
		}
	}
	return nil
}

func readCPULimit(files fs.FS) (*uint32, rt.MetricIssue) {
	unavailable := fieldReadFailed("cgroup cpu.max did not report the VMM scope quota")
	data, err := fs.ReadFile(files, "cpu.max")
	if err != nil {
		return nil, unavailable
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return nil, unavailable
	}
	period, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil || period == 0 {
		return nil, unavailable
	}
	if fields[0] == "max" {
		return nil, rt.MetricIssue{Code: "LIMIT_UNBOUNDED", Message: "VMM scope CPU quota is unbounded"}
	}
	quota, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil || quota == 0 || quota > math.MaxUint64/1000 || quota*1000/period > math.MaxUint32 {
		return nil, unavailable
	}
	millicpus := uint32(quota * 1000 / period)
	return &millicpus, rt.MetricIssue{}
}

func (a *Adapter) Capacity(ctx context.Context) (rt.Capacity, error) {
	result := rt.Capacity{MetricObservation: &rt.MetricObservation{
		Availability: "unavailable", Source: "worker_admission", Scope: "worker",
		Issues: map[string]rt.MetricIssue{
			"used_cpus":                unavailableMetric("Worker's ledger does not sample CPU usage"),
			"used_memory_mb":           unavailableMetric("Worker's ledger does not sample memory usage"),
			"used_disk_gb":             unavailableMetric("Worker's ledger does not sample disk usage"),
			"host_memory_total_mb":     unavailableMetric("Worker's admission limit is distinct from total host memory"),
			"host_memory_available_mb": unavailableMetric("Worker's ledger does not sample available host memory"),
			"boot_id":                  unavailableMetric("Worker's ledger does not identify a host boot"),
		},
	}}
	sampling := time.Now().UTC()
	client := workerclient.Client{Socket: a.WorkerSocket(), Bundle: a.route.Bundle}
	allocation, err := client.WorkerAllocations(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		issue := rt.MetricIssue{Code: "WORKER_BUDGET_UNAVAILABLE", Message: "the Worker admission ledger could not be read"}
		result.Issues["allocated_cpus"], result.Issues["allocated_memory_mb"] = issue, issue
		return result, nil
	}
	result.Availability, result.ObservedAt = "partial", &sampling
	result.AllocatedCPUs, result.AllocatedMemoryMB = *allocation.VCPUs, *allocation.MemoryMiB
	return result, nil
}
