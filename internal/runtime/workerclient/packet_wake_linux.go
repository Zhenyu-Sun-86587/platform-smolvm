package workerclient

import (
	"context"
	"encoding/json"
	"errors"
)

type PacketWakeEndpoint struct {
	AttachmentID string `json:"attachment_id"`
	NetworkID    string `json:"network_id"`
	PrivateIPv4  string `json:"private_ipv4"`
	GuestIPv4    string `json:"guest_ipv4"`
	DNSName      string `json:"dns_name"`
}

type PacketWakeRule struct {
	SourceAttachmentID string     `json:"source_attachment_id"`
	TargetAttachmentID string     `json:"target_attachment_id"`
	Transport          string     `json:"transport"`
	GuestPort          uint16     `json:"guest_port"`
	Enabled            *bool      `json:"enabled,omitempty"`
	ExpectedRevision   *int64     `json:"expected_revision,omitempty"`
	SourceExecution    *Execution `json:"source_execution,omitempty"`
}

func (c Client) LookupPacketWakeEndpoint(ctx context.Context, execution Execution) (PacketWakeEndpoint, error) {
	var endpoint PacketWakeEndpoint
	result, _, err := c.call(ctx, request{Method: "QueryPrivatePacketWakeEndpoint", Expected: &execution}, nil, nil)
	if err != nil {
		return endpoint, err
	}
	if err := json.Unmarshal(result, &endpoint); err != nil {
		return endpoint, err
	}
	if endpoint.AttachmentID == "" || endpoint.NetworkID == "" {
		return endpoint, errors.New("Worker returned an incomplete private wake endpoint")
	}
	return endpoint, nil
}

func (c Client) PacketWakeRule(ctx context.Context, execution Execution, rule PacketWakeRule, operationID, digest string) (json.RawMessage, error) {
	method := "QueryPrivatePacketWakeRule"
	if rule.Enabled != nil {
		method = "ApplyPrivatePacketWakeRule"
	}
	result, _, err := c.call(ctx, request{Method: method, Expected: &execution, Payload: rule,
		OperationID: operationID, RequestDigest: digest}, nil, nil)
	return result, err
}
