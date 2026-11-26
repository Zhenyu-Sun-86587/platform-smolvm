package platformapi

import (
	"context"
	"database/sql"
	"net"
	"regexp"
	"strings"

	"sandbox-platform/internal/lifecycle"
)

// Generated machine IDs have a fixed hex suffix. Using it keeps the browser
// origin stable across renames and distinct from service and console origins.
var webMachineID = regexp.MustCompile("^m_[0-9a-f]{32}$")

func machineWebHost(id, domain string) string {
	if domain == "" || !webMachineID.MatchString(id) {
		return ""
	}
	return "vm-" + strings.TrimPrefix(id, "m_") + "." + domain
}

func requestHostname(authority string) string {
	if host, _, err := net.SplitHostPort(authority); err == nil {
		authority = host
	}
	return strings.ToLower(authority)
}

func (s *Server) webMachine(host string) string {
	label, ok := strings.CutSuffix(host, "."+s.Config.Gateway.AppsDomain)
	if !ok || !strings.HasPrefix(label, "vm-") {
		return ""
	}
	id := "m_" + strings.TrimPrefix(label, "vm-")
	if machineWebHost(id, s.Config.Gateway.AppsDomain) != host {
		return ""
	}
	return id
}

func (s *Server) httpServiceForHost(ctx context.Context, host string) (service, error) {
	id := s.webMachine(host)
	if id == "" {
		return readService(s.DB.ReadDB().QueryRowContext(ctx, "SELECT "+serviceCols+" FROM services WHERE public_host=? AND kind IN ('http','https') AND enabled=1", host))
	}
	rows, err := s.DB.ReadDB().QueryContext(ctx, "SELECT "+serviceCols+` FROM services
		WHERE machine_id=? AND enabled=1 AND kind IN ('http','https') AND public_host IS NOT NULL
		AND EXISTS (SELECT 1 FROM machines WHERE id=services.machine_id AND deleted_at IS NULL)
		ORDER BY (name='web') DESC, id LIMIT 2`, id)
	if err != nil {
		return service{}, err
	}
	defer rows.Close()
	var target service
	for rows.Next() {
		candidate, err := readService(rows)
		if err != nil {
			return service{}, err
		}
		if target.ID == "" {
			target = candidate
		} else if target.Name != "web" {
			return service{}, &lifecycle.Failure{Code: "AMBIGUOUS_WEB_SERVICE", Message: "select a service URL or name exactly one HTTP service web"}
		}
	}
	if err := rows.Err(); err != nil {
		return service{}, err
	}
	if target.ID == "" {
		return service{}, sql.ErrNoRows
	}
	return target, nil
}
