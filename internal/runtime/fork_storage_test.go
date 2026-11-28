package runtime

import (
	"encoding/json"
	"testing"
)

func TestForkStorageUsesCompleteWritableInventory(t *testing.T) {
	var inventory NativeInventory
	raw := []byte(`{"execution":{"instance_id":"source","incarnation_id":"live","execution_epoch":4,"host_boot_id":"boot"},
	"inventory":{"devices":[{"virtio_type":2,"block":{"capacity_bytes":1024,"read_only":false,"managed_attachment":{
	"attachment_id":"a","device_id":"root","role":"root","volume_id":"root","generation_id":"base","writable_head_id":"head","grant_id":"writer","execution_epoch":4}}},
	{"virtio_type":2,"block":{"capacity_bytes":2048,"read_only":true,"managed_attachment":{
	"attachment_id":"b","device_id":"data","role":"data","volume_id":"data","generation_id":"sealed","execution_epoch":4}}}]}}`)
	if err := json.Unmarshal(raw, &inventory); err != nil {
		t.Fatal(err)
	}
	proof := inventory.ForkStorage()
	if proof == nil || proof.WritableBytes != 1024 || proof.HostBootID != "boot" || proof.Execution != inventory.Execution.ExecutionRef {
		t.Fatalf("writable capacity proof = %+v", proof)
	}
	inventory.Inventory.Devices[1].Block.CapacityBytes = 0
	if got := inventory.ForkStorage(); got != nil {
		t.Fatalf("incomplete inventory proved capacity: %+v", got)
	}
}
