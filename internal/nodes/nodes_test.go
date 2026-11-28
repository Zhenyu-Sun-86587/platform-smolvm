package nodes

import (
	"testing"

	"sandbox-platform/internal/lifecycle"
	"sandbox-platform/internal/runtime/workerclient"
)

func TestRebootRequiresOriginalHibernateCapture(t *testing.T) {
	pin := workerclient.Execution{InstanceID: "managed-slot", Epoch: 4}
	st := lifecycle.WorkerLifecycleStatus{Instance: pin.InstanceID, Epoch: 4, Power: "HIBERNATED", StatePoint: "capture"}
	if err := checkHibernatedBinding(pin, "capture", st); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []lifecycle.WorkerLifecycleStatus{
		{Instance: pin.InstanceID, Epoch: 4, Power: "STOPPED"},
		{Instance: pin.InstanceID, Epoch: 4, Power: "HIBERNATED", StatePoint: "different"},
		{Instance: pin.InstanceID, Epoch: 3, Power: "HIBERNATED", StatePoint: "capture"},
		{Instance: "foreign-slot", Epoch: 4, Power: "HIBERNATED", StatePoint: "capture"},
	} {
		if err := checkHibernatedBinding(pin, "capture", bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
