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
		ID                string `json:"resource_id"`
		State, Durability string
		Immutable         bool
		Import            string `json:"created_by_operation"`
		Descriptor        struct {
			Schema string
			Object struct {
				ID      string  `json:"resource_id"`
				Digest  string  `json:"content_sha256"`
				Bytes   int64   `json:"capacity_bytes"`
				Backing *string `json:"backing_id"`
			}
		}
	}
	if err = json.Unmarshal(raw, &r); err != nil {
		return 0, err
	}
	o := r.Descriptor.Object
	if r.ID != generation || o.ID != generation || r.State != "available" || !r.Immutable || r.Durability != "durable" ||
		r.Import == "" || r.Descriptor.Schema != "ManagedBlockObject/1" || o.Digest != digest || o.Backing != nil || o.Bytes <= 0 {
		return 0, errors.New("Worker did not confirm the immutable template root")
	}
	raw, _, err = c.call(ctx, request{Method: "QueryOperation", OperationID: r.Import}, nil, nil)
	if err != nil {
		return 0, err
	}
	var receipt struct {
		ID     string `json:"operation_id"`
		Phase  string
		Route  string `json:"route_id"`
		Bundle string `json:"bundle_digest"`
	}
	if err = json.Unmarshal(raw, &receipt); err != nil {
		return 0, err
	}
	if receipt.ID != r.Import || receipt.Phase != "COMPLETED" || receipt.Route != "deep" || receipt.Bundle != c.Bundle {
		return 0, errors.New("template import receipt belongs to another route or is incomplete")
	}
	return o.Bytes, nil
}
