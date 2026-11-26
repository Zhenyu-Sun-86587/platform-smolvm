package registry_test

import (
	"errors"
	"strings"
	"testing"

	"sandbox-platform/internal/nodeproto"
	"sandbox-platform/internal/registry"
)

func TestEstablishedWorkerRequiresOfflineJournalPin(t *testing.T) {
	ctx := t.Context()
	fixture := placementDB(t)
	sample := fixture.worker(t, "existing-worker")
	savePlacementSample(t, fixture.db, sample)
	control, err := fixture.db.Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := nodeproto.Authority{WorkerID: sample.WorkerID, EnrollmentGeneration: 1,
		ControllerID: control.InstallationID, ControlEpoch: control.ControlEpoch}
	original := nodeproto.NodeIdentity{JournalID: "journal-" + strings.Repeat("a", 64), NodeSession: sample.Session.NodeSessionID}
	replacement := nodeproto.NodeIdentity{JournalID: "journal-" + strings.Repeat("b", 64), NodeSession: "new-session"}
	if err := fixture.db.PinWorkerJournal(ctx, a, replacement, ""); !errors.Is(err, registry.ErrConflict) {
		t.Fatalf("established Worker silently adopted new journal: %v", err)
	}
	if _, err := fixture.db.WorkerNodeIdentity(ctx, a, nodeproto.NodeIdentity{}); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("established Worker served its old session without a journal pin: %v", err)
	}
	if err := fixture.db.PinWorkerJournal(ctx, a, original, original.JournalID); err != nil {
		t.Fatal(err)
	}
	if found, err := fixture.db.WorkerNodeIdentity(ctx, a, nodeproto.NodeIdentity{}); err != nil || found != original {
		t.Fatal(found, err)
	}
	if err := fixture.db.PinWorkerJournal(ctx, a, replacement, replacement.JournalID); !errors.Is(err, registry.ErrWorkerIdentityConflict) {
		t.Fatalf("replacing the operator config modified a pinned journal: %v", err)
	}
	if _, err := fixture.db.WorkerNodeIdentity(ctx, a, original); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("quarantined Worker remained authorized: %v", err)
	}
}
