package workerclient

import (
	"context"
	"encoding/json"
	"errors"
)

type statePointIdentity struct {
	CaptureID      string `json:"capture_id"`
	ManifestDigest string `json:"manifest_digest"`
}

func (c Client) ReleaseStatePoint(ctx context.Context, captureID, manifestDigest string) error {
	identity := statePointIdentity{CaptureID: captureID, ManifestDigest: manifestDigest}
	result, _, err := c.call(ctx, request{Method: "ReleaseStatePoint", Payload: identity}, nil, nil)
	if err != nil {
		return err
	}
	var receipt struct {
		statePointIdentity
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(result, &receipt); err != nil || receipt.statePointIdentity != identity || receipt.Phase != "released" {
		return errors.New("Worker StatePoint release receipt differs from the checkpoint")
	}
	return nil
}
