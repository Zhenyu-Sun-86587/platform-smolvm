package workerclient

import (
	"context"
	"encoding/json"
	"errors"

	rt "sandbox-platform/internal/runtime"
)

// QueryInstanceBinding reads the local catalog slot without asking Serve to
// register a Bundle or dispatch an operation. The caller verifies the Serve
// namespace before submitting the instance ID.
func (c Client) QueryInstanceBinding(ctx context.Context, instanceID string) (rt.LaunchBinding, error) {
	var binding rt.LaunchBinding
	if instanceID == "" {
		return binding, rt.ErrInvalid
	}
	raw, _, err := c.call(ctx, request{Method: "QueryInstanceBinding", Payload: map[string]string{"instance_id": instanceID}}, nil, nil)
	if err != nil {
		return binding, err
	}
	if err := json.Unmarshal(raw, &binding); err != nil {
		return binding, err
	}
	if binding.InstanceID != instanceID || binding.ExecutionEpoch < 0 ||
		(binding.IncarnationID != nil && (binding.ExecutionEpoch == 0 || *binding.IncarnationID == "")) {
		return rt.LaunchBinding{}, errors.New("Worker returned an invalid binding identity")
	}
	return binding, nil
}

// QueryOperation reads a scoped receipt from resourced without invoking a Serve
// reconciliation endpoint. The Node must also check the receipt's Serve namespace.
func (c Client) QueryOperation(ctx context.Context, id string) (rt.OperationReceipt, error) {
	var receipt rt.OperationReceipt
	raw, _, err := c.call(ctx, request{Method: "QueryOperation", OperationID: id}, nil, nil)
	if err != nil {
		return receipt, err
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return receipt, err
	}
	if id == "" || receipt.OperationID != id || receipt.RouteID != "deep" || receipt.BundleDigest != c.Bundle || receipt.Revision < 1 {
		return rt.OperationReceipt{}, errors.New("Worker operation receipt has a different scope")
	}
	return receipt, nil
}
