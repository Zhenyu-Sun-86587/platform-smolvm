package smolvm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"regexp"
	"time"
)

var ownedName = regexp.MustCompile(`^sbox_[a-f0-9]{32}$`)

// CheckRestartNever reads the official CLI's status JSON from the operator's
// read-only helper. HTTP MachineInfo does not expose this policy in v1.37.1.
func CheckRestartNever(ctx context.Context, socket, name, binding, state string) error {
	if socket == "" || !ownedName.MatchString(name) || binding == "" {
		return errors.New("managed runtime status helper or object identity is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, name+"\n"); err != nil {
		return err
	}
	if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
		return err
	}
	const limit = 64 << 10
	raw, err := io.ReadAll(io.LimitReader(conn, limit+1))
	if err != nil {
		return err
	}
	if len(raw) > limit {
		return errors.New("managed runtime status response exceeded limit")
	}
	var result struct {
		Name          string            `json:"name"`
		State         string            `json:"state"`
		RestartPolicy string            `json:"restart_policy"`
		Labels        map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return errors.New("managed runtime status helper did not return public status JSON")
	}
	if result.Name != name || result.State != state || result.Labels["platform.binding"] != binding {
		return errors.New("managed runtime CLI and HTTP observations differ")
	}
	if result.RestartPolicy != "never" {
		return errors.New("managed runtime restart policy is not never")
	}
	return nil
}
