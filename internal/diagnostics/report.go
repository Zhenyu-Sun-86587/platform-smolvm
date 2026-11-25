// Package diagnostics collects allowlisted local health and recovery evidence.
package diagnostics

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sys/unix"

	"sandbox-platform/internal/config"
	"sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/smolvm"
)

type Report struct {
	At            time.Time             `json:"at"`
	Expected      string                `json:"expected_runtime_version"`
	Runtime       *runtime.Health       `json:"runtime,omitempty"`
	RuntimeReady  bool                  `json:"runtime_ready"`
	Capacity      *runtime.Capacity     `json:"runtime_capacity,omitempty"`
	KVM           bool                  `json:"kvm_present"`
	CgroupV2      bool                  `json:"cgroup_v2"`
	Systemd       bool                  `json:"systemd_present"`
	Disk          *Disk                 `json:"disk,omitempty"`
	Pressure      map[string]string     `json:"host_pressure,omitempty"`
	Control       *Control              `json:"control,omitempty"`
	RuntimeVMs    []VM                  `json:"runtime_machines,omitempty"`
	Machines      []Binding             `json:"bindings,omitempty"`
	Operations    []Operation           `json:"operations,omitempty"`
	TaskCounts    map[string]int        `json:"task_counts,omitempty"`
	ServiceCounts map[string]int        `json:"service_counts,omitempty"`
	Logs          map[string]LogSummary `json:"journal_summary,omitempty"`
	Units         map[string]string     `json:"systemd_units,omitempty"`
	Checks        map[string]string     `json:"checks"`
	Config        map[string]any        `json:"effective_config"`
}

type Disk struct {
	AvailableBytes  uint64 `json:"available_bytes"`
	CapacityBytes   uint64 `json:"capacity_bytes"`
	AvailableInodes uint64 `json:"available_inodes"`
}

type Control struct {
	SchemaVersion int    `json:"schema_version"`
	Epoch         string `json:"control_epoch"`
	Maintenance   bool   `json:"maintenance"`
}

type VM struct {
	Name     string            `json:"runtime_name"`
	State    runtime.State     `json:"state"`
	Identity *runtime.Identity `json:"runtime_identity,omitempty"`
	Parent   string            `json:"parent_machine,omitempty"`
	Ports    []runtime.Port    `json:"ports"`
	Scope    string            `json:"scope,omitempty"`
}

type LogSummary struct {
	Entries int `json:"entries_sampled"`
	Errors  int `json:"error_or_higher"`
}

type Operation struct {
	ID      string `json:"operation_id"`
	Kind    string `json:"kind"`
	State   string `json:"state"`
	Effect  string `json:"effect"`
	Runtime string `json:"runtime_name,omitempty"`
}

type Binding struct {
	ID         string `json:"machine_id"`
	Generation int64  `json:"binding_generation"`
	Runtime    string `json:"runtime_name"`
	State      string `json:"observed_state"`
	Guest      string `json:"guest_management_status"`
}

func Collect(ctx context.Context, cfg config.Config) Report {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r := Report{At: time.Now().UTC(), Expected: cfg.Runtime.Version,
		Pressure: map[string]string{}, Units: map[string]string{}, Checks: map[string]string{},
		Config: map[string]any{"max_heavy_operations": cfg.Lifecycle.Heavy,
			"minimum_free_bytes": cfg.Storage.MinBytes, "minimum_free_percent": cfg.Storage.MinPercent,
			"default_idle_seconds": cfg.Lifecycle.IdleSeconds, "task_protocol_version": cfg.Tasks.Protocol,
			"tls_configured": cfg.API.TLSCert != "", "snapshot_encoding": cfg.Storage.Encoding},
	}
	if info, err := os.Stat("/dev/kvm"); err == nil {
		r.KVM = info.Mode()&os.ModeCharDevice != 0
	}
	_, err := os.Stat("/sys/fs/cgroup/cgroup.controllers")
	r.CgroupV2 = err == nil
	_, err = os.Stat("/run/systemd/system")
	r.Systemd = err == nil
	var disk unix.Statfs_t
	if err := unix.Statfs(cfg.StateDir, &disk); err == nil {
		r.Disk = &Disk{AvailableBytes: disk.Bavail * uint64(disk.Bsize),
			CapacityBytes: disk.Blocks * uint64(disk.Bsize), AvailableInodes: disk.Ffree}
	}
	for _, source := range []string{"cpu", "memory", "io"} {
		if pressure, err := os.ReadFile("/proc/pressure/" + source); err == nil {
			r.Pressure[source] = strings.TrimSpace(string(pressure))
		}
	}
	sm, err := smolvm.New(cfg.Runtime.Socket)
	if err != nil {
		r.Checks["runtime_socket"] = "invalid_configuration"
	} else if health, err := sm.Health(ctx); err != nil {
		r.Checks["runtime_socket"] = "unavailable"
	} else {
		r.Runtime = &health
		r.RuntimeReady = sm.Ready(ctx) == nil
		if capacity, err := sm.Capacity(ctx); err == nil {
			r.Capacity = &capacity
		}
		if machines, err := sm.List(ctx); err == nil {
			for _, m := range machines {
				r.RuntimeVMs = append(r.RuntimeVMs, VM{Name: m.Name, State: m.State, Identity: m.Identity, Parent: m.ParentMachine, Ports: m.Ports, Scope: scope(m.Identity)})
			}
		}
	}
	if r.Systemd {
		r.Logs = make(map[string]LogSummary)
		for _, unit := range []string{"platformd.service", "smolvm-serve.service"} {
			command := exec.CommandContext(ctx, "/usr/bin/systemctl", "show", "-p", "LoadState", "-p", "ActiveState", "-p", "MainPID", "-p", "ControlGroup", "-p", "User", "-p", "KillMode", unit)
			if details, err := command.Output(); err == nil {
				r.Units[unit] = strings.TrimSpace(string(details))
			}
			if summary, err := summarizeJournal(ctx, unit); err == nil {
				r.Logs[unit] = summary
			}
		}
	}
	r.collectDatabase(ctx, cfg.Database.Path)
	return r
}

func scope(identity *runtime.Identity) string {
	if !identity.Complete() {
		return ""
	}
	var pid int
	if err := json.Unmarshal(identity.PID, &pid); err != nil || pid <= 1 {
		return ""
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path
		}
	}
	return ""
}

func summarizeJournal(ctx context.Context, unit string) (LogSummary, error) {
	command := exec.CommandContext(ctx, "/usr/bin/journalctl", "--unit", unit, "--lines=100",
		"--no-pager", "--output=json", "--output-fields=PRIORITY")
	output, err := command.Output()
	var summary LogSummary
	if err != nil {
		return summary, err
	}
	reader := bufio.NewScanner(bytes.NewReader(output))
	for reader.Scan() {
		var entry struct {
			Priority string `json:"PRIORITY"`
		}
		if err := json.Unmarshal(reader.Bytes(), &entry); err != nil {
			return summary, err
		}
		summary.Entries++
		if level, err := strconv.Atoi(entry.Priority); err == nil && level <= 3 {
			summary.Errors++
		}
	}
	return summary, reader.Err()
}

func (r *Report) collectDatabase(ctx context.Context, path string) {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_busy_timeout=750"}
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		r.Checks["database"] = "unavailable"
		return
	}
	defer db.Close()
	var control Control
	if err := db.QueryRowContext(ctx, "SELECT schema_version, control_epoch, maintenance FROM meta WHERE id = 1").
		Scan(&control.SchemaVersion, &control.Epoch, &control.Maintenance); err != nil {
		r.Checks["database"] = "unavailable"
		return
	}
	r.Control = &control
	rows, err := db.QueryContext(ctx, "SELECT id, kind, state, effect, runtime_name FROM operations ORDER BY updated_at DESC LIMIT 100")
	if err == nil {
		for rows.Next() {
			var op Operation
			if rows.Scan(&op.ID, &op.Kind, &op.State, &op.Effect, &op.Runtime) == nil {
				r.Operations = append(r.Operations, op)
			}
		}
		rows.Close()
	}
	rows, err = db.QueryContext(ctx, "SELECT machine_id, generation, runtime_name, observed_state, guest_management_status FROM runtime_bindings WHERE status <> 'retired' ORDER BY machine_id LIMIT 1000")
	if err == nil {
		for rows.Next() {
			var binding Binding
			if rows.Scan(&binding.ID, &binding.Generation, &binding.Runtime, &binding.State, &binding.Guest) == nil {
				r.Machines = append(r.Machines, binding)
			}
		}
		rows.Close()
	}
	r.TaskCounts = aggregate(ctx, db, "SELECT state, count(*) FROM tasks GROUP BY state")
	r.ServiceCounts = aggregate(ctx, db, "SELECT kind, count(*) FROM services GROUP BY kind")
}

func aggregate(ctx context.Context, db *sql.DB, query string) map[string]int {
	result := make(map[string]int)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return result
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var count int
		if rows.Scan(&key, &count) == nil {
			result[key] = count
		}
	}
	return result
}

func WriteZip(output string, report Report) error {
	if output == "" {
		return errors.New("diagnostic output file is required")
	}
	f, err := os.CreateTemp(filepath.Dir(output), ".sandbox-diagnostics-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = writeZip(f, report)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	return unix.Renameat2(unix.AT_FDCWD, f.Name(), unix.AT_FDCWD, output, unix.RENAME_NOREPLACE)
}

func writeZip(output io.Writer, report Report) error {
	archive := zip.NewWriter(output)
	f, err := archive.Create("report.json")
	if err == nil {
		encoder := json.NewEncoder(f)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(report)
	}
	return errors.Join(err, archive.Close())
}
