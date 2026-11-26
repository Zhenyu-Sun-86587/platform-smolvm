package workerclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

// CPULimitReceipt describes the kernel cpu.max readback for a single VMM scope.
type CPULimitReceipt struct {
	Execution
	RequestedMillicpus *uint32 `json:"requested_millicpus"`
	QuotaUsec          *uint64 `json:"quota_usec"`
	PeriodUsec         uint64  `json:"period_usec"`
}

func (c Client) ManagedCPULimit(ctx context.Context, ref Execution) (CPULimitReceipt, error) {
	raw, _, err := c.call(ctx, request{Method: "QueryManagedCPULimit", Expected: &ref, Payload: struct{}{}}, nil, nil)
	if err != nil {
		return CPULimitReceipt{}, err
	}
	return cpuLimitReadback(raw, ref)
}

func cpuLimitReadback(raw json.RawMessage, ref Execution) (CPULimitReceipt, error) {
	var receipt CPULimitReceipt
	var nullable struct {
		Limit json.RawMessage `json:"requested_millicpus"`
		Quota json.RawMessage `json:"quota_usec"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return receipt, err
	}
	if err := json.Unmarshal(raw, &nullable); err != nil {
		return receipt, err
	}
	if receipt.Execution != ref || receipt.PeriodUsec < 1000 || receipt.PeriodUsec > 1_000_000 ||
		len(nullable.Limit) == 0 || len(nullable.Quota) == 0 || receipt.QuotaUsec != nil && *receipt.QuotaUsec == 0 {
		return receipt, errors.New("Worker CPU limit receipt does not identify a valid execution and kernel quota")
	}
	return receipt, nil
}

func (c Client) SetManagedCPULimit(ctx context.Context, operation string, ref Execution, millicpus *uint32) (CPULimitReceipt, error) {
	var receipt CPULimitReceipt
	if operation == "" || millicpus != nil && *millicpus == 0 {
		return receipt, errors.New("CPU limit requires an operation and positive millicpus or null")
	}
	payload := struct {
		Millicpus *uint32 `json:"millicpus"`
	}{millicpus}
	requestBody, err := json.Marshal([]any{"SetManagedCPULimit", operation, ref, payload})
	if err != nil {
		return receipt, err
	}
	raw, _, err := c.call(ctx, request{Method: "SetManagedCPULimit", OperationID: operation,
		RequestDigest: fmt.Sprintf("sha256:%x", sha256.Sum256(requestBody)), Expected: &ref, Payload: payload}, nil, nil)
	if err != nil {
		return receipt, err
	}
	if receipt, err = cpuLimitReadback(raw, ref); err != nil {
		return receipt, err
	}
	if (millicpus == nil) != (receipt.RequestedMillicpus == nil) {
		return receipt, errors.New("Worker reported a different requested CPU limit")
	}
	if millicpus != nil {
		numerator := uint64(*millicpus) * receipt.PeriodUsec
		if *receipt.RequestedMillicpus != *millicpus || receipt.QuotaUsec == nil ||
			numerator%1000 != 0 || *receipt.QuotaUsec != numerator/1000 {
			return receipt, errors.New("kernel CPU quota differs from the requested millicpus")
		}
	}
	return receipt, nil
}
