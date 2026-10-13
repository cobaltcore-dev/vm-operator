// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

// hypervisorTypeExtraSpec records which hypervisor a flavor targets; only qemu
// and ch are managed.
const hypervisorTypeExtraSpec = "capabilities:hypervisor_type"

// ConvertedVirtualMachine is the VirtualMachine a Nova server maps onto, built by
// ConvertToVirtualMachine. It embeds the fully-populated VirtualMachine so callers
// read Spec.AZ, Spec.Resources, etc. directly.
//
// Only the Nova-derived fields are populated: conditions, Status.Nova.LastSyncTime
// and Status.Host are owned elsewhere.
type ConvertedVirtualMachine struct {
	v1alpha1.VirtualMachine
}

// ErrUnmanagedHypervisorType is returned by ConvertToVirtualMachine when the
// server's flavor is not a hypervisor type this operator manages (qemu/ch) — the
// one expected, non-fatal reason to skip a server. Callers test for it with
// errors.Is.
var ErrUnmanagedHypervisorType = errors.New("nova server flavor is not a managed hypervisor type")

// ConvertToVirtualMachine maps a Nova server + flavor onto a VirtualMachine. It
// returns ErrUnmanagedHypervisorType when the flavor is not a hypervisor type
// this operator manages (qemu/ch) — including a nil flavor, e.g. a
// boot-from-volume or not-yet-scheduled server — so callers can skip such servers.
func ConvertToVirtualMachine(server *servers.Server, flavor *flavors.Flavor, region string) (ConvertedVirtualMachine, error) {
	// Only qemu/ch flavors are managed; anything else (including a nil flavor) is
	// skipped by the caller via ErrUnmanagedHypervisorType.
	var hypervisorType v1alpha1.HypervisorType
	if flavor != nil {
		switch strings.ToLower(strings.TrimSpace(flavor.ExtraSpecs[hypervisorTypeExtraSpec])) {
		case "qemu":
			hypervisorType = v1alpha1.HypervisorTypeQEMU
		case "ch":
			hypervisorType = v1alpha1.HypervisorTypeCH
		}
	}
	if hypervisorType == "" {
		return ConvertedVirtualMachine{}, fmt.Errorf("nova server %s: %w", server.ID, ErrUnmanagedHypervisorType)
	}

	// CreatedAt stays nil for the zero time so an unset timestamp is not stored as
	// an epoch value; the image id is a loosely-typed map lookup (empty when the
	// server booted from volume).
	var createdAt *metav1.Time
	if !server.Created.IsZero() {
		createdAt = new(metav1.NewTime(server.Created))
	}
	imageUUID, _ := server.Image["id"].(string)

	return ConvertedVirtualMachine{v1alpha1.VirtualMachine{
		// The CRD is named after the Nova server uuid.
		ObjectMeta: metav1.ObjectMeta{Name: server.ID},
		Spec: v1alpha1.VMSpec{
			AZ:             server.AvailabilityZone,
			Region:         region,
			HypervisorType: &hypervisorType,
			TargetHost:     &v1alpha1.HostRef{Name: server.Host},
			Resources: map[v1alpha1.ResourceName]resource.Quantity{
				v1alpha1.ResourceCPU: *resource.NewQuantity(int64(flavor.VCPUs), resource.DecimalSI),
				// RAM is reported in MB; ResourceMemory is bytes.
				v1alpha1.ResourceMemory: *resource.NewQuantity(int64(flavor.RAM)*1024*1024, resource.BinarySI),
			},
			Nova: v1alpha1.NovaSpec{
				InstanceName: server.InstanceName,
				CreatedAt:    createdAt,
				Ownership:    v1alpha1.Ownership{Project: server.TenantID, User: server.UserID},
				Flavor:       v1alpha1.Flavor{Name: flavor.Name, FlavorID: flavor.ID, ExtraSpecs: flavor.ExtraSpecs},
				Image:        v1alpha1.Image{UUID: imageUUID},
			},
		},
		Status: v1alpha1.VMStatus{
			// Status.Host (the runtime host) is owned elsewhere; the nova host goes to
			// Status.Nova.Host.
			Nova: &v1alpha1.NovaStatus{Status: server.Status, Host: server.Host},
		},
	}}, nil
}

// HasChanged reports whether applying this conversion to vm would change any
// sync-owned field — i.e. vm is out of sync. It compares Spec.Nova (excluding
// Flavor, which never drives a sync on its own) and Status.Nova.Status, ignoring
// LastSyncTime, conditions and Status.Host.
func (c ConvertedVirtualMachine) HasChanged(vm *v1alpha1.VirtualMachine) bool {
	want := c.Spec.Nova
	got := vm.Spec.Nova
	want.Flavor, got.Flavor = v1alpha1.Flavor{}, v1alpha1.Flavor{}
	// A nil Status.Nova compares as empty so a never-synced resource is clean.
	var gotStatus, gotHost string
	if vm.Status.Nova != nil {
		gotStatus, gotHost = vm.Status.Nova.Status, vm.Status.Nova.Host
	}
	return !equality.Semantic.DeepEqual(want, got) ||
		c.Status.Nova.Status != gotStatus || c.Status.Nova.Host != gotHost
}

// PatchInto applies the sync-owned fields (Spec.Nova and Status.Nova) onto vm,
// leaving the immutable spec, conditions, Status.Host and LastSyncTime untouched.
func (c ConvertedVirtualMachine) PatchInto(vm *v1alpha1.VirtualMachine) {
	var lastSync *metav1.Time
	if vm.Status.Nova != nil {
		lastSync = vm.Status.Nova.LastSyncTime // preserved so cleanup can measure orphan TTL.
	}
	vm.Spec.Nova = c.Spec.Nova
	vm.Status.Nova = &v1alpha1.NovaStatus{
		Status:       c.Status.Nova.Status,
		Host:         c.Status.Nova.Host,
		LastSyncTime: lastSync,
	}
}
