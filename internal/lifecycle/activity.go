package lifecycle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/runtime/workerclient"
	"sandbox-platform/internal/tasks"
)

// Inspect each network namespace visible to the agent, including container
// namespaces. A disappeared process is retried on the next sample.
const activityProbe = `set -eu
seen=" "
for proc in /proc/[0-9]*; do
  ns=$(readlink "$proc/ns/net") || { [ ! -d "$proc" ] && continue; exit 1; }
  case "$seen" in *" $ns "*) continue;; esac
  seen="$seen$ns "
  for protocol in tcp tcp6 udp udp6; do
    file="$proc/net/$protocol"
    if [ ! -f "$file" ]; then case "$protocol" in *6) continue;; *) exit 1;; esac; fi
    printf 'TABLE %s %s\n' "$ns" "$protocol"
    cat "$file" || exit 1
  done
done
printf 'END\n'`

type socketRow struct {
	key, local, remote, state string
	udp                       bool
}

func socketCounts(raw []byte) (int, int, bool) {
	if !bytes.HasSuffix(raw, []byte("END\n")) {
		return 0, 0, false
	}
	var key string
	udp := false
	listeners := map[string]bool{}
	var rows []socketRow
	for _, line := range strings.Split(string(raw), "\n") {
		words := strings.Fields(line)
		if len(words) == 0 || words[0] == "sl" || words[0] == "END" {
			continue
		}
		if words[0] == "TABLE" && len(words) == 3 {
			key = words[1] + "/" + words[2] + "/"
			udp = strings.HasPrefix(words[2], "udp")
			continue
		}
		if key == "" || len(words) < 4 || !strings.HasSuffix(words[0], ":") {
			return 0, 0, false
		}
		local := strings.Split(words[1], ":")
		peer := strings.Split(words[2], ":")
		if len(local) != 2 || len(peer) != 2 {
			return 0, 0, false
		}
		if words[3] == "0A" {
			listeners[key+local[1]] = true
			continue
		}
		rows = append(rows, socketRow{key, local[1], peer[1], words[3], udp})
	}
	if key == "" {
		return 0, 0, false
	}
	in, out := 0, 0
	for _, s := range rows {
		if s.udp && s.remote == "0000" || !s.udp && s.state != "01" && s.state != "02" && s.state != "03" && s.state != "08" {
			continue
		}
		if listeners[s.key+s.local] {
			in++
		} else {
			out++
		}
	}
	return in, out, true
}
func (e *Engine) activitySession() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.collectorSession == "" {
		e.collectorSession, _ = registry.NewID("collector-")
	}
	return e.collectorSession
}
func (e *Engine) reportActivity(ctx context.Context, m registry.Machine, b registry.Binding, adapter rt.Adapter, client workerclient.Client, st WorkerLifecycleStatus, manager *tasks.Hello) {
	scope := WorkerExecution(b)
	if st.Execution() != scope || scope.IncarnationID == "" {
		return
	}
	owner := e.activitySession()
	if owner == "" {
		return
	}
	// The Worker retires a collector session when its execution changes.
	// Keep the same ID for retries within this incarnation.
	identity := scope.IncarnationID
	if st.CollectorGeneration != "" {
		identity += "\x00" + st.CollectorGeneration
	}
	digest := sha256.Sum256([]byte(owner + "\x00" + identity))
	session := "collector-" + hex.EncodeToString(digest[:16])
	publish := func(name string, counts map[string]any) {
		previous := st.Collectors[name]
		seq := previous.Seq + 1
		params := map[string]any{"collector": name, "session": session, "seq": seq, "valid_for_ms": 20000, "counts": counts}
		if st.CollectorGeneration != "" {
			params["generation"] = st.CollectorGeneration
		}
		if previous.Session != "" && previous.Session != session {
			params["supersedes"] = previous.Session
			seq = 1
			params["seq"] = seq
		}
		key := name + "-" + session + "-" + b.ID + "-" + strconv.FormatInt(scope.Epoch, 10) + "-" + strconv.FormatInt(seq, 10)
		_ = client.Lifecycle(ctx, "ReportActivity", key, scope, params, nil)
	}
	host := map[string]any{"inbound": nil, "outbound": nil}
	var known map[string]int64
	if err := client.Lifecycle(ctx, "QueryHostActivity", "", scope, map[string]any{}, &known); err == nil {
		if incoming, present := known["inbound"]; present {
			host["inbound"] = incoming
		}
		if outgoing, present := known["outbound"]; present {
			host["outbound"] = outgoing
		}
	}
	publish("host", host)
	platform := map[string]any{"task": nil, "exec": nil, "stream": nil, "legacy_lease": nil}
	var taskCount, execCount, legacyLeases int
	if err := e.DB.ReadDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM leases WHERE machine_id=? AND binding_generation=? AND worker_revision IS NULL AND semantics_version=1 AND state IN ('active','suspect')", m.ID, b.Generation).Scan(&legacyLeases); err == nil {
		platform["legacy_lease"] = legacyLeases
	}
	if err := e.DB.ReadDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM tasks WHERE machine_id=? AND binding_generation=? AND state NOT IN ('exited','completed','succeeded','failed','cancelled','interrupted')", m.ID, b.Generation).Scan(&taskCount); err == nil {
		platform["task"] = taskCount
	}
	if err := e.DB.ReadDB().QueryRowContext(ctx, "SELECT COUNT(*) FROM exec_sessions WHERE machine_id=? AND binding_id=? AND state IN ('reserved','starting','running')", m.ID, b.ID).Scan(&execCount); err == nil {
		platform["exec"] = execCount
	}
	active, _ := e.AccessCounts(m.ID)
	platform["stream"] = active
	publish("platform", platform)
	if st.Power != "RUNNING" {
		return
	}
	guest := map[string]any{"task": nil, "exec": nil, "inbound": nil, "outbound": nil}
	control, ok := adapter.(rt.NativeGuestControl)
	if ok {
		sample, stop := context.WithTimeout(ctx, 4*time.Second)
		defer stop()
		res, err := control.GuestExec(sample, b.ExecutionRef(), rt.ExecRequest{Command: []string{"/bin/sh", "-c", activityProbe}, TimeoutSeconds: 2})
		if err == nil && res.ExitCode == 0 {
			if in, out, ok := socketCounts(res.Stdout); ok {
				guest["inbound"] = in
				guest["outbound"] = out
			}
		}
	}
	if manager != nil && manager.Unknown == 0 && manager.Barrier == nil {
		guest["task"] = manager.Active
		if platform["exec"] != nil {
			guest["exec"] = execCount
		}
	}
	publish("guest", guest)
}
