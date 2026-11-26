package workerclient

import (
	"context"
	"encoding/json"
	"errors"

	"sandbox-platform/internal/placement"
)

type Health struct {
	State           string `json:"state"`
	HostBootID      string `json:"host_boot_id"`
	BrokerSessionID string `json:"broker_session_id"`
}

func (c Client) Health(ctx context.Context) (Health, error) {
	var h Health
	raw, _, err := c.call(ctx, request{Method: "Health"}, nil, nil)
	if err != nil {
		return h, err
	}
	if err = json.Unmarshal(raw, &h); err != nil {
		return h, err
	}
	if h.State != "ready" || h.HostBootID == "" || h.BrokerSessionID == "" {
		return h, errors.New("Worker catalog has not established a ready session")
	}
	return h, nil
}

func (c Client) Budget(ctx context.Context) (placement.Budget, error) {
	type vector struct {
		VCPU    *int64 `json:"vcpus"`
		Memory  *int64 `json:"memory_mib"`
		Storage *int64 `json:"storage_bytes"`
	}
	var fields struct{ Capacity, Allocated vector }
	var b placement.Budget
	raw, _, err := c.call(ctx, request{Method: "QueryWorkerBudget"}, nil, nil)
	if err != nil {
		return b, err
	}
	if err = json.Unmarshal(raw, &fields); err != nil {
		return b, err
	}
	for _, p := range []struct {
		v   vector
		dst *placement.Resources
	}{{fields.Capacity, &b.Capacity}, {fields.Allocated, &b.Allocated}} {
		if p.v.VCPU == nil || p.v.Memory == nil || p.v.Storage == nil {
			return b, errors.New("Worker budget is incomplete")
		}
		p.dst.VCPUs, p.dst.MemoryMiB, p.dst.StorageBytes = *p.v.VCPU, *p.v.Memory, *p.v.Storage
		if !p.dst.Valid() {
			return b, errors.New("invalid Worker budget")
		}
	}
	return b, nil
}

// VerifyRoot accepts a registered candidate generation and confirms the immutable
// catalog entry and its import operation in this Bundle. It returns only the size.
func (c Client) VerifyRoot(ctx context.Context, generation, digest string) (int64, error) {
	raw, _, err := c.call(ctx, request{Method: "QueryResource", Payload: map[string]string{"resource_id": generation}}, nil, nil)
	if err != nil {
		return 0, err
	}
	var r struct {
		ID         string `json:"resource_id"`
		Kind       string `json:"kind"`
		State      string `json:"state"`
		Durability string `json:"durability"`
		Immutable  bool   `json:"immutable"`
		Import     string `json:"created_by_operation"`
		Descriptor struct {
			Schema string `json:"schema"`
			Object struct {
				ID      string  `json:"resource_id"`
				Digest  string  `json:"content_sha256"`
				Bytes   int64   `json:"capacity_bytes"`
				Backing *string `json:"backing_id"`
				Volume  *string `json:"volume_id"`
				Format  string  `json:"format"`
			} `json:"object"`
		} `json:"descriptor"`
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return 0, err
	}
	o := r.Descriptor.Object
	if r.ID != generation || o.ID != generation || r.Kind != "descriptor" || r.State != "available" || !r.Immutable || r.Durability != "durable" ||
		r.Import == "" || r.Descriptor.Schema != "ManagedBlockObject/1" || o.Digest != digest || o.Backing != nil || o.Volume != nil ||
		o.Format != "raw" || o.Bytes <= 0 || o.Bytes%512 != 0 {
		return 0, errors.New("Worker did not confirm the immutable template root")
	}
	receipt, err := c.QueryOperation(ctx, r.Import)
	if err != nil {
		return 0, err
	}
	if receipt.Phase != "COMPLETED" {
		return 0, errors.New("template import receipt belongs to another route or is incomplete")
	}
	return o.Bytes, nil
}
