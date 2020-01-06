/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vcenter

import (
	"github.com/pkg/errors"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/cloud/vsphere/context"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/cloud/vsphere/services/govmomi/extra"
	"sigs.k8s.io/cluster-api-provider-vsphere/pkg/cloud/vsphere/services/govmomi/template"
)

const (
	diskMoveType        = string(types.VirtualMachineRelocateDiskMoveOptionsMoveAllDiskBackingsAndConsolidate)
	snapShotName        = "CAPVSnapshot"
	snapShotDescription = "Pre clone snapshot created by cluster-api-provider-vsphere"
)

// Clone kicks off a clone operation on vCenter to create a new virtual machine.
func Clone(ctx *context.MachineContext, bootstrapData []byte) error {
	ctx = context.NewMachineLoggerContext(ctx, "vcenter")
	ctx.Logger.V(6).Info("starting clone process")

	var extraConfig extra.Config
	extraConfig.SetCloudInitUserData(bootstrapData)

	tpl, err := template.FindTemplate(ctx, ctx.VSphereMachine.Spec.Template)
	if err != nil {
		return err
	}

	folder, err := ctx.Session.Finder.FolderOrDefault(ctx, ctx.VSphereCluster.Spec.CloudProviderConfiguration.Workspace.Folder)
	if err != nil {
		return errors.Wrapf(err, "unable to get folder for %q", ctx)
	}

	datastore, err := ctx.Session.Finder.DatastoreOrDefault(ctx, ctx.VSphereCluster.Spec.CloudProviderConfiguration.Workspace.Datastore)
	if err != nil {
		return errors.Wrapf(err, "unable to get datastore for %q", ctx)
	}

	pool, err := ctx.Session.Finder.ResourcePoolOrDefault(ctx, ctx.VSphereCluster.Spec.CloudProviderConfiguration.Workspace.ResourcePool)
	if err != nil {
		return errors.Wrapf(err, "unable to get resource pool for %q", ctx)
	}

	devices, err := tpl.Device(ctx)

	if err != nil {
		return errors.Wrapf(err, "error getting devices for %q", ctx)
	}

	diskSpec, err := getDiskSpec(ctx, devices)
	if err != nil {
		return errors.Wrapf(err, "error getting disk spec for %q", ctx)
	}

	networkSpecs, err := getNetworkSpecs(ctx, devices)
	if err != nil {
		return errors.Wrapf(err, "error getting network specs for %q", ctx)
	}

	deviceSpecs := []types.BaseVirtualDeviceConfigSpec{diskSpec}
	deviceSpecs = append(deviceSpecs, networkSpecs...)

	numCPUs := ctx.VSphereMachine.Spec.NumCPUs
	if numCPUs < 2 {
		numCPUs = 2
	}
	numCoresPerSocket := ctx.VSphereMachine.Spec.NumCoresPerSocket
	if numCoresPerSocket == 0 {
		numCoresPerSocket = numCPUs
	}
	memMiB := ctx.VSphereMachine.Spec.MemoryMiB
	if memMiB == 0 {
		memMiB = 2048
	}
	spec := types.VirtualMachineCloneSpec{
		Config: &types.VirtualMachineConfigSpec{
			Annotation: ctx.String(),
			// Assign the clone's InstanceUUID the value of the Kubernetes Machine
			// object's UID. This allows lookup of the cloned VM prior to knowing
			// the VM's UUID.
			InstanceUuid:      string(ctx.Machine.UID),
			Flags:             newVMFlagInfo(),
			DeviceChange:      deviceSpecs,
			ExtraConfig:       extraConfig,
			NumCPUs:           numCPUs,
			NumCoresPerSocket: numCoresPerSocket,
			MemoryMB:          memMiB,
		},
		Location: types.VirtualMachineRelocateSpec{
			Datastore:    types.NewReference(datastore.Reference()),
			DiskMoveType: diskMoveType,
			Folder:       types.NewReference(folder.Reference()),
			Pool:         types.NewReference(pool.Reference()),
		},
		// This is implicit, but making it explicit as it is important to not
		// power the VM on before its virtual hardware is created and the MAC
		// address(es) used to build and inject the VM with cloud-init metadata
		// are generated.
		PowerOn: false,
	}
	// https://pubs.vmware.com/vsphere-50/index.jsp?topic=%2Fcom.vmware.wssdk.pg.doc_50%2FPG_Ch11_VM_Manage.13.4.html
	if ctx.VSphereMachine.Spec.LinkedClone {
		snapshot, err := ensureSnapshot(ctx, tpl, *pool)
		if err != nil {
			return errors.Wrapf(err, "error ensuring snapshot for machine %q", ctx)
		}
		spec.Snapshot = snapshot
		spec.Location.DiskMoveType = string(types.VirtualMachineRelocateDiskMoveOptionsCreateNewChildDiskBacking)
	}

	ctx.Logger.V(6).Info("cloning machine", "clone-spec", spec)
	task, err := tpl.Clone(ctx, folder, ctx.Machine.Name, spec)
	if err != nil {
		return errors.Wrapf(err, "error trigging clone op for machine %q", ctx)
	}

	ctx.VSphereMachine.Status.TaskRef = task.Reference().Value

	return nil
}

func newVMFlagInfo() *types.VirtualMachineFlagInfo {
	diskUUIDEnabled := true
	return &types.VirtualMachineFlagInfo{
		DiskUuidEnabled: &diskUUIDEnabled,
	}
}

func getDiskSpec(
	ctx *context.MachineContext,
	devices object.VirtualDeviceList) (types.BaseVirtualDeviceConfigSpec, error) {

	disks := devices.SelectByType((*types.VirtualDisk)(nil))
	if len(disks) != 1 {
		return nil, errors.Errorf("invalid disk count: %d", len(disks))
	}

	disk := disks[0].(*types.VirtualDisk)
	disk.CapacityInKB = int64(ctx.VSphereMachine.Spec.DiskGiB) * 1024 * 1024

	return &types.VirtualDeviceConfigSpec{
		Operation: types.VirtualDeviceConfigSpecOperationEdit,
		Device:    disk,
	}, nil
}

const ethCardType = "vmxnet3"

func getNetworkSpecs(
	ctx *context.MachineContext,
	devices object.VirtualDeviceList) ([]types.BaseVirtualDeviceConfigSpec, error) {

	deviceSpecs := []types.BaseVirtualDeviceConfigSpec{}

	// Remove any existing NICs
	for _, dev := range devices.SelectByType((*types.VirtualEthernetCard)(nil)) {
		deviceSpecs = append(deviceSpecs, &types.VirtualDeviceConfigSpec{
			Device:    dev,
			Operation: types.VirtualDeviceConfigSpecOperationRemove,
		})
	}

	// Add new NICs based on the machine config.
	key := int32(-100)
	for i := range ctx.VSphereMachine.Spec.Network.Devices {
		netSpec := &ctx.VSphereMachine.Spec.Network.Devices[i]
		ref, err := ctx.Session.Finder.Network(ctx, netSpec.NetworkName)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to find network %q", netSpec.NetworkName)
		}
		backing, err := ref.EthernetCardBackingInfo(ctx)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to create new ethernet card backing info for network %q on %q", netSpec.NetworkName, ctx)
		}
		dev, err := object.EthernetCardTypes().CreateEthernetCard(ethCardType, backing)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to create new ethernet card %q for network %q on %q", ethCardType, netSpec.NetworkName, ctx)
		}

		// Get the actual NIC object. This is safe to assert without a check
		// because "object.EthernetCardTypes().CreateEthernetCard" returns a
		// "types.BaseVirtualEthernetCard" as a "types.BaseVirtualDevice".
		nic := dev.(types.BaseVirtualEthernetCard).GetVirtualEthernetCard()

		if netSpec.MACAddr != "" {
			nic.MacAddress = netSpec.MACAddr
			// Please see https://www.vmware.com/support/developer/converter-sdk/conv60_apireference/vim.vm.device.VirtualEthernetCard.html#addressType
			// for the valid values for this field.
			nic.AddressType = string(types.VirtualEthernetCardMacTypeManual)
			ctx.Logger.V(6).Info("configured manual mac address", "mac-addr", nic.MacAddress)
		}

		// Assign a temporary device key to ensure that a unique one will be
		// generated when the device is created.
		nic.Key = key

		deviceSpecs = append(deviceSpecs, &types.VirtualDeviceConfigSpec{
			Device:    dev,
			Operation: types.VirtualDeviceConfigSpecOperationAdd,
		})
		ctx.Logger.V(6).Info("created network device", "eth-card-type", ethCardType, "network-spec", netSpec)
		key--
	}

	return deviceSpecs, nil
}

func ensureSnapshot(ctx *context.MachineContext, tpl *object.VirtualMachine, pool object.ResourcePool) (*types.ManagedObjectReference, error) {
	var ref mo.VirtualMachine

	if err := tpl.Properties(ctx, tpl.Reference(), []string{"snapshot", "summary.config.template"}, &ref); err != nil {
		return nil, errors.Wrapf(err, "unable to retrieve snapshot info for template %q on %q", tpl, ctx)
	}

	// do nothing if a snapshot exists
	if ref.Snapshot != nil {
		return ref.Snapshot.CurrentSnapshot, nil
	}
	// check if this is a template
	if ref.Summary.Config.Template {
		if err := tpl.MarkAsVirtualMachine(ctx, pool, nil); err != nil {
			return nil, errors.Wrapf(err, "unable to mark template as virtual machine for template %q on %q", tpl, ctx)
		}
	}
	ctx.Logger.V(6).Info("creating snapshot", "snapShotName", snapShotName)
	task, err := tpl.CreateSnapshot(ctx, snapShotName, snapShotDescription, false, false)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to create snapshot for template %q on %q", tpl, ctx)
	}
	taskInfo, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to create snapshot for template %q on %q", tpl, ctx)
	}

	// convert back to template if original ref was a template
	if ref.Summary.Config.Template {
		if err := tpl.MarkAsTemplate(ctx); err != nil {
			return nil, errors.Wrapf(err, "unable to mark as template %q on %q", tpl, ctx)
		}
	}
	result := taskInfo.Result.(types.ManagedObjectReference)
	return &result, nil

}
