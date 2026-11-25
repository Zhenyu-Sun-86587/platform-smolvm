package customsmolvm

import (
	"context"
	"fmt"
	"regexp"

	rt "sandbox-platform/internal/runtime"
)

type OperationReceipt struct {
	OperationID            string  `json:"operation_id"`
	Kind                   string  `json:"kind"`
	RequestDigest          string  `json:"request_digest"`
	RouteID                string  `json:"route_id"`
	InstanceID             string  `json:"instance_id"`
	BundleDigest           string  `json:"bundle_digest"`
	ExpectedExecutionEpoch int64   `json:"expected_execution_epoch"`
	ExpectedIncarnationID  *string `json:"expected_incarnation_id"`
	Phase                  string  `json:"phase"`
	Decision               string  `json:"decision"`
	Revision               int64   `json:"revision"`
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// QueryOperation looks up the original durable intent after a lost response.
// It never re-dispatches the operation.
func QueryOperation(ctx context.Context, socket, bundle, reader, id string) (OperationReceipt, error) {
	var receipt OperationReceipt
	if !identifier.MatchString(id) {
		return receipt, fmt.Errorf("%w: invalid operation ID", rt.ErrInvalid)
	}
	if _, err := Probe(ctx, socket, bundle, reader); err != nil {
		return receipt, err
	}
	if err := get(ctx, socket, extensionPath+"/operations/"+id, &receipt); err != nil {
		return OperationReceipt{}, err
	}
	if receipt.OperationID != id || receipt.BundleDigest != bundle || receipt.RouteID != "deep" ||
		receipt.Kind == "" || !identifier.MatchString(receipt.InstanceID) || !validDigest(receipt.RequestDigest) ||
		receipt.Revision < 1 || receipt.Phase == "" || receipt.Decision == "" || receipt.ExpectedExecutionEpoch < 0 ||
		(receipt.ExpectedIncarnationID != nil && (!identifier.MatchString(*receipt.ExpectedIncarnationID) || receipt.ExpectedExecutionEpoch == 0)) {
		return OperationReceipt{}, fmt.Errorf("%w: incompatible durable operation receipt", rt.ErrUnsupported)
	}
	return receipt, nil
}
