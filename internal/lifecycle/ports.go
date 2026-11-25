package lifecycle

import (
	"context"
	"net"
	"sandbox-platform/internal/registry"
	rt "sandbox-platform/internal/runtime"
	"strconv"
)

// RepublishServices prepares and verifies loopback ports on a replacement.
// Gateway continues resolving the old binding until the caller's atomic switch.
func (j *Job) RepublishServices(ctx context.Context) error {
	rows, err := j.DB.ReadDB().QueryContext(ctx, "SELECT id,guest_port FROM services WHERE machine_id=? AND enabled=1 ORDER BY id", j.Op.MachineID)
	if err != nil {
		return err
	}
	var ports []rt.Port
	var ids []string
	for rows.Next() {
		var id string
		var port rt.Port
		if err = rows.Scan(&id, &port.Guest); err != nil {
			rows.Close()
			return err
		}
		port.Protocol = "tcp"
		ports = append(ports, port)
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(ids) == 0 {
		return err
	}
	if err = j.Stage(ctx, "prepare_service_routes", ""); err != nil {
		return err
	}
	tx, err := j.DB.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rng := j.Config.Gateway.BackendRange
	candidate := rng[0]
	for i, id := range ids {
		for ; candidate <= rng[1]; candidate++ {
			var used int
			if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM port_allocations WHERE protocol='tcp' AND host_port=? AND state!='released'", candidate).Scan(&used); err != nil {
				return err
			}
			if used != 0 {
				continue
			}
			l, e := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(candidate)))
			if e != nil {
				continue
			}
			l.Close()
			ports[i].Host = uint16(candidate)
			candidate++
			break
		}
		if ports[i].Host == 0 {
			return &Failure{"RESOURCE_EXHAUSTED", "no free service port"}
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO port_allocations(id,binding_id,service_id,protocol,host_port,guest_port,state) VALUES(?,?,?,'tcp',?,?,'allocated')", "port_"+j.Op.ID+id, j.Binding.ID, id, ports[i].Host, ports[i].Guest)
		if err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if _, err = j.Runtime.SetPorts(ctx, j.Binding.RuntimeName, ports); err != nil {
		return err
	}
	o, err := j.Runtime.Observe(ctx, j.Binding.RuntimeName)
	if err != nil {
		return err
	}
	if len(o.Ports) != len(ids) || o.Labels["platform.binding"] != j.Binding.ID {
		return registry.ErrStaleObservation
	}
	seen := map[rt.Port]bool{}
	for _, p := range o.Ports {
		seen[p] = true
	}
	for _, p := range ports {
		if !seen[p] {
			return registry.ErrStaleObservation
		}
	}
	_, err = j.DB.DB().ExecContext(ctx, "UPDATE port_allocations SET state='bound' WHERE binding_id=? AND state='allocated'", j.Binding.ID)
	return err
}
