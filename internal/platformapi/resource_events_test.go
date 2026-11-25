package platformapi

import (
	"testing"

	"sandbox-platform/internal/auth"
	"sandbox-platform/internal/registry"
)

func TestEventAudienceFollowsOwnerAndMachineScope(t *testing.T) {
	volume := registry.Event{Type: "operation", ResourceKind: "volume", Audience: "owner", AudienceOwner: "owner"}
	owner := auth.Principal{ID: "owner", Scope: auth.Scope{Access: auth.AccessRead}}
	other := auth.Principal{ID: "other", Scope: auth.Scope{Access: auth.AccessManage}}
	guest := auth.Principal{ID: "owner", Scope: auth.Scope{Access: auth.AccessRead, MachineID: "machine"}}
	if !permitsEvent(owner, volume) || permitsEvent(other, volume) || permitsEvent(guest, volume) {
		t.Fatal("volume event differs from its owner scope")
	}
	volume.MachineID = "machine"
	if !permitsEvent(guest, volume) {
		t.Fatal("owner's machine-scoped volume event was hidden")
	}
	if permitsEvent(owner, registry.Event{Type: "auth", Audience: "legacy"}) || permitsEvent(guest, registry.Event{Type: "auth", Audience: "admin"}) {
		t.Fatal("limited token can observe an administrator event")
	}
}
