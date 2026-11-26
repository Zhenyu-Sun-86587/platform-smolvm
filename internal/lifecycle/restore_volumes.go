package lifecycle

import (
	"context"

	rt "sandbox-platform/internal/runtime"
	"sandbox-platform/internal/volumes"
)

// The Worker creates product attachments when it finishes an independent
// restore. Check those references against the actual registered target before
// publishing the volume graph and its machine binding in one transaction.
func (j *Job) observeRestoredVolumes(ctx context.Context, inventory rt.NativeInventory, point rt.StatePointProgress) ([]volumes.Volume, error) {
	if len(point.ProductAttachments) == 0 {
		return nil, nil
	}
	invalid := &Failure{"RECOVERY_REQUIRED", "restored product volume does not match the confirmed execution"}
	control, ok := j.Runtime.(rt.VolumeControl)
	if !ok {
		return nil, invalid
	}
	route := rt.VolumeRoute{RuntimeInstanceID: j.Binding.RuntimeInstanceID, RuntimeBundleID: j.Binding.RuntimeBundleID}
	disks := make(map[string]*rt.NativeBlock)
	for _, device := range inventory.Inventory.Devices {
		if block := device.Block; block != nil && block.ManagedAttachment != nil {
			disks[block.ManagedAttachment.VolumeID] = block
		}
	}
	sources := make(map[string]string)
	for source, target := range point.VolumeMap {
		sources[target] = source
	}
	result := make([]volumes.Volume, 0, len(point.ProductAttachments))
	for volume, attachment := range point.ProductAttachments {
		disk := disks[volume]
		if disk == nil || sources[volume] == "" {
			return nil, invalid
		}
		source, err := volumes.Get(ctx, j.DB.ReadDB(), j.Op.PrincipalID, sources[volume])
		if err != nil {
			return nil, err
		}
		view, err := control.ObserveVolume(ctx, volume, j.Op.PrincipalID)
		if err != nil {
			return nil, err
		}
		if source.VolumeRoute != route || view.State != "available" || (!disk.ReadOnly && view.HeadResourceID != disk.ManagedAttachment.WritableHeadID) {
			return nil, invalid
		}
		matched := false
		for _, lease := range view.Attachments {
			if lease.ID != attachment {
				continue
			}
			matched = lease.InstanceID == inventory.Execution.InstanceID && lease.State == "attached" &&
				lease.DeviceKey == disk.ManagedAttachment.DeviceID && (lease.Mode == "ro") == disk.ReadOnly &&
				(!disk.ReadOnly || lease.GenerationID == disk.ManagedAttachment.GenerationID)
			break
		}
		if !matched {
			return nil, invalid
		}
		result = append(result, volumes.Volume{VolumeRoute: route, Name: source.Name, VolumeObservation: view})
	}
	return result, nil
}
