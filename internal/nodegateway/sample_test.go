package nodegateway

import (
	"context"
	"slices"
	"testing"
	"time"

	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/placement"
)

func TestWarmingRootsLeaveTimeForBudgetAndRotate(t *testing.T) {
	first, cancelFirst := context.WithCancel(t.Context())
	defer cancelFirst()
	var order []string
	verify := func(ctx context.Context, generation, digest string) (int64, error) {
		order = append(order, generation)
		switch generation {
		case "a-warming":
			<-ctx.Done()
			return 0, ctx.Err()
		case "b-cut":
			cancelFirst()
			return 0, context.Canceled
		default:
			return 512, nil
		}
	}
	var checks []rootCheck
	for _, id := range []string{"c-ready", "a-warming", "b-cut"} {
		checks = append(checks, rootCheck{key: id, artifact: nodeproto.Artifact{Generation: id, Digest: id}, verify: verify})
	}
	cursor := ""
	profiles := []placement.RuntimeProfile{{RuntimeInstanceID: "serve"}}
	receipts := map[string][]nodeproto.Artifact{}
	started := time.Now()
	sampleRoots(first, checks, &cursor, profiles, receipts)
	if time.Since(started) >= placement.MaxQueryDuration/2 || cursor != "b-cut" || len(receipts) != 0 {
		t.Fatalf("warming roots exhausted budget time or produced a false proof: %s, %v", cursor, receipts)
	}
	sampleRoots(t.Context(), checks, &cursor, profiles, receipts)
	got := receipts["serve"]
	if !slices.Equal(order, []string{"a-warming", "b-cut", "c-ready", "a-warming", "b-cut"}) ||
		len(got) != 1 || got[0].Generation != "c-ready" || got[0].Bytes != 512 || len(profiles[0].Artifacts) != 1 {
		t.Fatalf("only the ready root should be schedulable: %v, probe order %v", got, order)
	}
}
