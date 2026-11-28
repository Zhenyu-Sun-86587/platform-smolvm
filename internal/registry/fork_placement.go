package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"

	"sandbox-platform/internal/placement"
	rt "sandbox-platform/internal/runtime"
)

func setForkPlacementStorage(ctx context.Context, tx *sql.Tx, b *Binding, route rt.BundleRoute, children int64, request *placement.Request) error {
	var health struct {
		Storage *rt.ForkStorageEvidence `json:"native_fork_storage"`
	}
	missing := placementEvidenceMissing(route, "fork_storage_evidence_unavailable")
	if json.Unmarshal(b.Health, &health) != nil || health.Storage == nil ||
		health.Storage.Execution != b.ExecutionRef() || children <= 0 ||
		health.Storage.WritableBytes <= 0 || health.Storage.WritableBytes > math.MaxInt64/children {
		return missing
	}
	sample, err := readWorkerSample(ctx, tx, route.Owner())
	if errors.Is(err, ErrNotFound) {
		return missing
	}
	if err != nil {
		return err
	}
	if sample.Session.HostBootID != health.Storage.HostBootID {
		return missing
	}
	request.Resources.StorageBytes = health.Storage.WritableBytes * children
	return nil
}

// Recheck the accepted writable footprint against current live inventory
// before sending the first Fork request. A native retry uses the original
// child holds, and does not require the source to still be live.
func (s *Store) VerifyForkPlacementStorage(ctx context.Context, operation string, children int, evidence *rt.ForkStorageEvidence) error {
	r, err := readPlacementReservation(ctx, s.reader, operation)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if children <= 0 || evidence == nil || evidence.WritableBytes <= 0 ||
		r.Resources.StorageBytes%int64(children) != 0 || r.Resources.StorageBytes/int64(children) != evidence.WritableBytes {
		return &rt.Error{Code: "FORK_STORAGE_EVIDENCE_CHANGED",
			Message: "Worker's current writable device capacity differs from the accepted Fork reservation"}
	}
	return nil
}
