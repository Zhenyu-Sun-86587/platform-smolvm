package registry

import (
	"context"
	"errors"
	"fmt"

	rt "sandbox-platform/internal/runtime"
)

var ErrWorkerMaintenance = errors.New("Worker entrance closed for maintenance")

func workerModeAllows(mode, entrance string) bool {
	switch mode {
	case "active":
		return true
	case "cordoned":
		return !newWorkerPlacement(entrance)
	case "draining", "drained":
		return !newWorkerPlacement(entrance) && !oneOf(entrance, "start", rt.Wake, "access", "service", "packet_wake_enable")
	default:
		return false
	}
}

func newWorkerPlacement(kind string) bool {
	return rt.IsNativeFork(kind) || oneOf(kind, "create", "fork", "branch", "clone", "restore", rt.VMRestore, "fork_retry")
}

func checkWorkerEntrance(ctx context.Context, q queryer, worker, entrance string) error {
	if worker == "" {
		worker = "local-worker"
	}
	var mode, phase string
	err := q.QueryRowContext(ctx, `SELECT w.desired_mode,COALESCE(m.phase,'') FROM workers w
		LEFT JOIN worker_maintenance m ON m.worker_id=w.id WHERE w.id=?`, worker).Scan(&mode, &phase)
	if err != nil {
		return notFound(err)
	}
	if phase == "activating" && !workerModeAllows("draining", entrance) || !workerModeAllows(mode, entrance) {
		return fmt.Errorf("%w: Worker %s is %s", ErrWorkerMaintenance, worker, mode)
	}
	return nil
}

func (s *Store) CheckWorkerEntrance(ctx context.Context, worker, entrance string) error {
	return checkWorkerEntrance(ctx, s.reader, worker, entrance)
}
