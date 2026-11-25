package customsmolvm

import (
	"context"
	"fmt"
	"net/http"
	"regexp"

	rt "sandbox-platform/internal/runtime"
)

type OperationReceipt = rt.OperationReceipt

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// QueryOperation looks up the original durable intent after a lost response.
// It never re-dispatches the operation.
func QueryOperation(ctx context.Context, socket, bundle, reader, id string) (OperationReceipt, error) {
	if err := validateEndpoint(socket, bundle, reader); err != nil {
		return OperationReceipt{}, err
	}
	if !identifier.MatchString(id) {
		return OperationReceipt{}, fmt.Errorf("%w: invalid operation ID", rt.ErrInvalid)
	}
	endpoint := dialEndpoint(socket)
	defer endpoint.CloseIdleConnections()
	if _, err := probe(ctx, endpoint, bundle, reader); err != nil {
		return OperationReceipt{}, err
	}
	return queryOperation(ctx, endpoint, bundle, id)
}

func queryOperation(ctx context.Context, endpoint *http.Client, bundle, id string) (OperationReceipt, error) {
	var receipt OperationReceipt
	if !identifier.MatchString(id) {
		return receipt, fmt.Errorf("%w: invalid operation ID", rt.ErrInvalid)
	}
	if err := get(ctx, endpoint, extensionPath+"/operations/"+id, &receipt); err != nil {
		return OperationReceipt{}, err
	}
	if err := validateReceipt(receipt, bundle, id); err != nil {
		return OperationReceipt{}, err
	}
	return receipt, nil
}

func validateReceipt(receipt OperationReceipt, bundle, id string) error {
	if receipt.OperationID != id || receipt.BundleDigest != bundle || receipt.RouteID != "deep" ||
		receipt.Kind == "" || !identifier.MatchString(receipt.InstanceID) || !validDigest(receipt.RequestDigest) ||
		receipt.Revision < 1 || receipt.Phase == "" || receipt.Decision == "" || receipt.ExpectedExecutionEpoch < 0 ||
		(receipt.ExpectedIncarnationID != nil && (!identifier.MatchString(*receipt.ExpectedIncarnationID) || receipt.ExpectedExecutionEpoch == 0)) {
		return fmt.Errorf("%w: incompatible durable operation receipt", rt.ErrUnsupported)
	}
	return nil
}
