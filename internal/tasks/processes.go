package tasks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const serviceDir = "/usr/local/share/sandbox-managed/services"

type CaptureProcess struct {
	PID       int    `json:"pid"`
	StartTime string `json:"start_time"`
	Role      string `json:"role"`
}

type snapshotProcess struct {
	pid, parent int
	start       string
	exe         string
	argv        []string
	cgroup      string
}

func readProcess(pid int) (snapshotProcess, error) {
	proc := filepath.Join("/proc", strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(proc, "stat"))
	if err != nil {
		return snapshotProcess{}, err
	}
	last := strings.LastIndexByte(string(stat), ')')
	if last < 0 {
		return snapshotProcess{}, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(stat[last+1:]))
	if len(fields) < 20 {
		return snapshotProcess{}, errors.New("incomplete process stat")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return snapshotProcess{}, os.ErrNotExist
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return snapshotProcess{}, err
	}
	exe, err := os.Readlink(filepath.Join(proc, "exe"))
	if err != nil {
		return snapshotProcess{}, err
	}
	cmd, err := os.ReadFile(filepath.Join(proc, "cmdline"))
	if err != nil {
		return snapshotProcess{}, err
	}
	cg, err := os.ReadFile(filepath.Join(proc, "cgroup"))
	if err != nil {
		return snapshotProcess{}, err
	}
	group, ok := strings.CutPrefix(strings.TrimSpace(string(cg)), "0::")
	if !ok || !filepath.IsAbs(group) || strings.Contains(group, "\n") {
		return snapshotProcess{}, errors.New("unexpected process cgroup")
	}
	start, err := processStart(pid)
	if err != nil || start != fields[19] {
		return snapshotProcess{}, os.ErrNotExist
	}
	return snapshotProcess{pid: pid, parent: parent, start: start, exe: exe,
		argv: strings.Split(strings.TrimRight(string(cmd), "\x00"), "\x00"), cgroup: group}, nil
}

func (m *manager) captureProcesses(frozen []FrozenTask) ([]CaptureProcess, error) {
	var services map[string]string
	path := filepath.Join(filepath.Dir(serviceDir), "capture-services.json")
	if err := readJSON(path, &services); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read authorized capture services: %w", err)
	}
	for name, executable := range services {
		if !identifier.MatchString(name) || name == "taskd" || name == "sshd" || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
			return nil, errors.New("invalid authorized capture service")
		}
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	all := make(map[int]snapshotProcess)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || !entry.IsDir() || pid < 1 {
			continue
		}
		p, err := readProcess(pid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect guest process %d: %w", pid, err)
		}
		all[pid] = p
	}

	roles := make(map[int]string)
	supervisors := make(map[int]string)
	configured := make(map[int]string)
	for pid, p := range all {
		if p.exe == "/usr/bin/runsvdir" && len(p.argv) == 2 && p.argv[1] == serviceDir {
			roles[pid] = "managed_supervisor"
		}
	}
	for pid, p := range all {
		if roles[p.parent] == "managed_supervisor" && p.exe == "/usr/bin/runsv" && len(p.argv) == 2 {
			name := p.argv[1]
			if name == "sshd" || name == "taskd" || services[name] != "" {
				supervisors[pid] = name
				roles[pid] = "service_supervisor"
			}
		}
	}
	for pid, p := range all {
		service := supervisors[p.parent]
		switch {
		case p.pid == 1 && p.exe == "/run/smolvm/init":
			roles[pid] = "guest_init"
		case p.pid == 2 && p.exe == "/usr/bin/sleep" && len(p.argv) == 2 && p.argv[1] == "infinity":
			roles[pid] = "guest_keepalive"
		case pid == os.Getpid() && service == "taskd":
			roles[pid] = "task_manager"
		case p.exe == TaskctlPath && len(p.argv) == 2 && p.argv[1] == "rpc":
			roles[pid] = "task_control"
		case service == "sshd" && p.exe == "/usr/sbin/sshd" && strings.Contains(p.argv[0], "[listener]"):
			roles[pid] = "ssh_listener"
		case service != "" && p.exe == services[service]:
			configured[pid] = service
		}
	}

	processes := make([]CaptureProcess, 0, len(all))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		p, exists := all[pid]
		if err != nil || !exists {
			continue
		}
		role := roles[pid]
		if role == "" {
			role = m.captureWorkloadRole(p, frozen)
		}
		if role == "" {
			for parent := p; parent.pid > 0; parent = all[parent.parent] {
				if service := configured[parent.pid]; service != "" {
					role = "service:" + service
					break
				}
			}
		}
		if role == "" {
			return nil, rpcError("TASK_CAPTURE_UNMANAGED_PROCESS",
				fmt.Sprintf("guest process %d (%s) has no verified capture role", pid, filepath.Base(p.exe)))
		}
		processes = append(processes, CaptureProcess{PID: pid, StartTime: p.start, Role: role})
	}
	return processes, nil
}

func (m *manager) captureWorkloadRole(p snapshotProcess, frozen []FrozenTask) string {
	for _, task := range frozen {
		if p.pid == task.Identity.PID && p.start == task.Identity.StartTime && p.exe == m.runner {
			return "task_runner"
		}
		group := filepath.Base(task.Identity.Cgroup)
		if p.cgroup == "/"+group || strings.HasPrefix(p.cgroup, "/"+group+"/") {
			return "task_payload"
		}
	}
	return ""
}
