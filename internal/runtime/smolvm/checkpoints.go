package smolvm

import (
	"context"
	"io"
	"net/http"
	"net/url"

	"sandbox-platform/internal/runtime"
)

const checkpointType = "application/vnd.smolmachines.checkpoint"

// CaptureCheckpoint streams a native standalone snapshot to the caller's staging file.
// A successful stream still needs the catalog's integrity and durability checks.
func (client *Client) CaptureCheckpoint(ctx context.Context, name string, dst io.Writer) (int64, error) {
	path, err := machinePath(name, "/checkpoint")
	if err != nil {
		return 0, err
	}
	return client.stream(ctx, http.MethodPost, path, url.Values{"incremental": {"false"}}, dst, checkpointType)
}

func (client *Client) ExportPausedCheckpoint(ctx context.Context, name string, dst io.Writer, pauseID ...string) (int64, error) {
	path, err := machinePath(name, "/pause/checkpoint")
	if err != nil {
		return 0, err
	}
	var query url.Values
	if len(pauseID) > 1 || (len(pauseID) == 1 && !pauseIDPattern.MatchString(pauseID[0])) {
		return 0, invalid("invalid native pause ID")
	}
	if len(pauseID) == 1 {
		query = url.Values{"operation_id": {pauseID[0]}}
	}
	return client.stream(ctx, http.MethodGet, path, query, dst, checkpointType)
}

// RestoreCheckpoint registers a previously verified managed artifact with
// outbound traffic restricted. Inherited ports must be cleared and verified
// by the caller before its explicit Start.
func (client *Client) RestoreCheckpoint(ctx context.Context, name string, source io.Reader, length int64) (runtime.Observation, error) {
	path, err := machinePath(name, "/checkpoint")
	if err != nil {
		return runtime.Observation{}, err
	}
	if source == nil || length == 0 || length < -1 {
		return runtime.Observation{}, invalid("missing checkpoint source or invalid length")
	}
	query := url.Values{"ports": {"[]"}, "allowed_cidrs": {"[]"}, "allowed_hosts": {"[]"}, "egress_rules": {"[]"}}
	response, err := client.request(ctx, http.MethodPut, path, query, binaryType, source, length)
	if err != nil {
		return runtime.Observation{}, err
	}
	defer response.Body.Close()
	var result machineInfo
	if err := decode(response.Body, &result, true); err != nil {
		return runtime.Observation{}, err
	}
	observation, err := result.expected(name, true)
	if err == nil && observation.State != runtime.StateCreated {
		err = protocolError("checkpoint registration did not return a created runtime", true, nil)
	}
	return observation, err
}
