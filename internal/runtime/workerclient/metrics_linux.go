package workerclient

import (
	"context"
	"encoding/json"
	"errors"
)

// WorkerAllocations are reservations in the shared Worker admission ledger.
// They carry no CPU or memory usage information.
type WorkerAllocations struct {
	VCPUs     *uint32 `json:"vcpus"`
	MemoryMiB *uint64 `json:"memory_mib"`
}

func (c Client) WorkerAllocations(ctx context.Context) (WorkerAllocations, error) {
	var reply struct {
		Allocated WorkerAllocations `json:"allocated"`
	}
	raw, _, err := c.call(ctx, request{Method: "QueryWorkerBudget"}, nil, nil)
	if err != nil {
		return reply.Allocated, err
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return WorkerAllocations{}, err
	}
	if reply.Allocated.VCPUs == nil || reply.Allocated.MemoryMiB == nil {
		return WorkerAllocations{}, errors.New("Worker budget did not report CPU and memory reservations")
	}
	return reply.Allocated, nil
}
