// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"errors"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

func fullServer() *servers.Server {
	return &servers.Server{
		ID:           testVMUUID,
		Status:       "ACTIVE",
		Host:         "compute-1",
		TenantID:     "project-uuid",
		UserID:       "user-uuid",
		InstanceName: "web-1",
		Created:      time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC),
		Image:        map[string]any{"id": "image-uuid"},
	}
}

// mustConvert converts server+flavor, failing the test on an unexpected error.
func mustConvert(t *testing.T, server *servers.Server, flavor *flavors.Flavor) ConvertedVirtualMachine {
	t.Helper()
	conv, err := ConvertToVirtualMachine(server, flavor, "qa-de-1")
	if err != nil {
		t.Fatalf("ConvertToVirtualMachine: %v", err)
	}
	return conv
}

func TestConvertToVirtualMachine_errorsOnUnmanagedFlavor(t *testing.T) {
	server := &servers.Server{ID: "a", Status: "ACTIVE"}
	if _, err := ConvertToVirtualMachine(server, qemuFlavor("f", 1, 1), ""); err != nil {
		t.Errorf("qemu flavor must convert, got %v", err)
	}
	if _, err := ConvertToVirtualMachine(server, nil, ""); !errors.Is(err, ErrUnmanagedHypervisorType) {
		t.Errorf("a nil flavor must return ErrUnmanagedHypervisorType, got %v", err)
	}
	unmanaged := &flavors.Flavor{ExtraSpecs: map[string]string{hypervisorTypeExtraSpec: "vmware"}}
	if _, err := ConvertToVirtualMachine(server, unmanaged, ""); !errors.Is(err, ErrUnmanagedHypervisorType) {
		t.Errorf("an unmanaged hypervisor type must return ErrUnmanagedHypervisorType, got %v", err)
	}
}

func TestPatchInto_populates(t *testing.T) {
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	mustConvert(t, fullServer(), qemuFlavor("flavor-uuid", 2, 2048)).PatchInto(vm)

	if vm.Spec.Nova.InstanceName != "web-1" {
		t.Errorf("instanceName = %q, want web-1", vm.Spec.Nova.InstanceName)
	}
	if vm.Spec.Nova.CreatedAt == nil {
		t.Error("expected CreatedAt to be set")
	}
	if vm.Spec.Nova.Ownership.Project != "project-uuid" || vm.Spec.Nova.Ownership.User != "user-uuid" {
		t.Errorf("ownership = %+v", vm.Spec.Nova.Ownership)
	}
	if vm.Spec.Nova.Flavor.Name != "flavor-uuid" || vm.Spec.Nova.Flavor.FlavorID != "flavor-uuid" {
		t.Errorf("flavor = %+v", vm.Spec.Nova.Flavor)
	}
	if vm.Spec.Nova.Image.UUID != "image-uuid" {
		t.Errorf("image = %+v", vm.Spec.Nova.Image)
	}
	if vm.Status.Nova == nil || vm.Status.Nova.Status != "ACTIVE" {
		t.Errorf("status.nova = %+v", vm.Status.Nova)
	}
	// PatchInto owns neither the immutable spec nor Status.Host.
	if vm.Spec.AZ != "" || vm.Status.Host != nil {
		t.Errorf("PatchInto must not set immutable spec or status.host: az=%q host=%+v", vm.Spec.AZ, vm.Status.Host)
	}
}

func TestPatchInto_preservesLastSyncTime(t *testing.T) {
	ts := new(metav1.Now())
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	vm.Status.Nova = &v1alpha1.NovaStatus{Status: "OLD", LastSyncTime: ts}

	mustConvert(t, fullServer(), qemuFlavor("flavor-uuid", 1, 1)).PatchInto(vm)

	if vm.Status.Nova.LastSyncTime != ts {
		t.Error("PatchInto must preserve LastSyncTime (Reconcile owns it)")
	}
}

// TestPatchInto_preservesCortex locks in that the Nova sync never clobbers the
// Cortex subspec, which the Cortex placement API owns.
func TestPatchInto_preservesCortex(t *testing.T) {
	cortex := &v1alpha1.CortexSpec{
		HostCandidates: []corev1.ObjectReference{{Kind: "Hypervisor", Name: "compute-1"}},
	}
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	vm.Spec.Cortex = cortex

	mustConvert(t, fullServer(), qemuFlavor("flavor-uuid", 1, 1)).PatchInto(vm)

	if vm.Spec.Cortex != cortex {
		t.Errorf("PatchInto must not touch Spec.Cortex: got %+v", vm.Spec.Cortex)
	}
}

func TestHasChanged(t *testing.T) {
	server := &servers.Server{ID: "a", Status: "ACTIVE", Host: "compute-1"}

	inSync := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	mustConvert(t, server, qemuFlavor("f", 2, 2048)).PatchInto(inSync)
	if mustConvert(t, server, qemuFlavor("f", 2, 2048)).HasChanged(inSync) {
		t.Error("a freshly-patched VM must not have changed")
	}

	// LastSyncTime must not count as a change.
	inSync.Status.Nova.LastSyncTime = new(metav1.Now())
	if mustConvert(t, server, qemuFlavor("f", 2, 2048)).HasChanged(inSync) {
		t.Error("LastSyncTime must not make a VM look changed")
	}

	// A status change must be detected.
	changed := &servers.Server{ID: "a", Status: "SHUTOFF", Host: "compute-1"}
	if !mustConvert(t, changed, qemuFlavor("f", 2, 2048)).HasChanged(inSync) {
		t.Error("a status change must be detected")
	}

	// A flavor difference alone must NOT drive a change.
	if mustConvert(t, server, qemuFlavor("other", 8, 9999)).HasChanged(inSync) {
		t.Error("flavor alone must not make a VM look changed")
	}
}

func TestConvertToVirtualMachine_setsNameAndImmutableSpec(t *testing.T) {
	conv := mustConvert(t,
		&servers.Server{ID: "a", Status: "BUILD", Host: "compute-1", AvailabilityZone: "qa-de-1a"},
		qemuFlavor("flavor-uuid", 4, 8192))

	vm := conv.DeepCopy()

	if vm.Name != "a" {
		t.Errorf("name = %q, want a (the server uuid)", vm.Name)
	}
	if vm.Spec.AZ != "qa-de-1a" || vm.Spec.Region != "qa-de-1" {
		t.Errorf("az/region = %q/%q", vm.Spec.AZ, vm.Spec.Region)
	}
	if vm.Spec.HypervisorType == nil || *vm.Spec.HypervisorType != v1alpha1.HypervisorTypeQEMU {
		t.Errorf("hypervisorType = %v, want QEMU", vm.Spec.HypervisorType)
	}
	if vm.Spec.TargetHost == nil || vm.Spec.TargetHost.Name != "compute-1" {
		t.Errorf("targetHost = %+v", vm.Spec.TargetHost)
	}
	// The nova host goes to Status.Nova.Host; Status.Host (runtime host) is owned elsewhere.
	if vm.Status.Nova == nil || vm.Status.Nova.Host != "compute-1" {
		t.Errorf("status.nova.host = %+v, want compute-1", vm.Status.Nova)
	}
	if vm.Status.Host != nil {
		t.Errorf("status.host = %+v, want nil (not owned by the nova syncer)", vm.Status.Host)
	}
	if cpu := vm.Spec.Resources[v1alpha1.ResourceCPU]; cpu.Value() != 4 {
		t.Errorf("resources[cpu] = %s, want 4", cpu.String())
	}
	if mem := vm.Spec.Resources[v1alpha1.ResourceMemory]; mem.Value() != 8192*1024*1024 {
		t.Errorf("resources[memory] = %s, want 8192Mi", mem.String())
	}
	if vm.Spec.Nova.Flavor.Name != "flavor-uuid" {
		t.Errorf("flavor = %+v", vm.Spec.Nova.Flavor)
	}
}

func TestConvertToVirtualMachine_hypervisorType(t *testing.T) {
	spec := func(v string) *flavors.Flavor {
		return &flavors.Flavor{ID: "f", ExtraSpecs: map[string]string{hypervisorTypeExtraSpec: v}}
	}
	tests := []struct {
		name   string
		flavor *flavors.Flavor
		want   v1alpha1.HypervisorType // "" means the conversion must be rejected as unmanaged.
	}{
		{"qemu", spec("qemu"), v1alpha1.HypervisorTypeQEMU},
		{"ch", spec("ch"), v1alpha1.HypervisorTypeCH},
		{"uppercase and spaces", spec("  QEMU "), v1alpha1.HypervisorTypeQEMU},
		{"unmanaged value", spec("vmware"), ""},
		{"missing extra spec", &flavors.Flavor{ID: "f", ExtraSpecs: map[string]string{}}, ""},
		{"nil extra specs", &flavors.Flavor{ID: "f"}, ""},
		{"nil flavor", nil, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conv, err := ConvertToVirtualMachine(&servers.Server{ID: "a", Status: "ACTIVE"}, tc.flavor, "")
			if tc.want == "" {
				if !errors.Is(err, ErrUnmanagedHypervisorType) {
					t.Errorf("want ErrUnmanagedHypervisorType, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ConvertToVirtualMachine: %v", err)
			}
			if conv.Spec.HypervisorType == nil || *conv.Spec.HypervisorType != tc.want {
				t.Errorf("hypervisorType = %v, want %q", conv.Spec.HypervisorType, tc.want)
			}
		})
	}
}
