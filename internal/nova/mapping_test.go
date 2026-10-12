// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

func fullServer() *servers.Server {
	return &servers.Server{
		ID:           "63ed64e2-27a2-4b70-9b5c-833c2a285d66",
		Status:       "ACTIVE",
		Host:         "compute-1",
		TenantID:     "project-uuid",
		UserID:       "user-uuid",
		InstanceName: "web-1",
		Created:      time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC),
		Flavor: map[string]any{
			"original_name": "m1.small",
			"id":            "flavor-uuid",
			"extra_specs":   map[string]any{"hw:cpu_policy": "dedicated"},
		},
		Image: map[string]any{"id": "image-uuid"},
	}
}

func TestApplyNovaState_populates(t *testing.T) {
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	applyNovaState(vm, fullServer())

	if vm.Spec.Nova.InstanceName != "web-1" {
		t.Errorf("instanceName = %q, want web-1", vm.Spec.Nova.InstanceName)
	}
	if vm.Spec.Nova.CreatedAt == nil {
		t.Error("expected CreatedAt to be set")
	}
	if vm.Spec.Nova.Ownership.Project != "project-uuid" || vm.Spec.Nova.Ownership.User != "user-uuid" {
		t.Errorf("ownership = %+v", vm.Spec.Nova.Ownership)
	}
	if vm.Spec.Nova.Flavor.Name != "m1.small" || vm.Spec.Nova.Flavor.FlavorID != "flavor-uuid" {
		t.Errorf("flavor = %+v", vm.Spec.Nova.Flavor)
	}
	if vm.Spec.Nova.Flavor.ExtraSpecs["hw:cpu_policy"] != "dedicated" {
		t.Errorf("extra specs = %+v", vm.Spec.Nova.Flavor.ExtraSpecs)
	}
	if vm.Spec.Nova.Image.UUID != "image-uuid" {
		t.Errorf("image = %+v", vm.Spec.Nova.Image)
	}
	if vm.Status.Nova == nil || vm.Status.Nova.Status != "ACTIVE" {
		t.Errorf("status.nova = %+v", vm.Status.Nova)
	}
	if vm.Status.Host == nil || vm.Status.Host.Name != "compute-1" {
		t.Errorf("status.host = %+v", vm.Status.Host)
	}
}

func TestApplyNovaState_preservesLastSyncTime(t *testing.T) {
	ts := new(metav1.Now())
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	vm.Status.Nova = &v1alpha1.NovaStatus{Status: "OLD", LastSyncTime: ts}

	applyNovaState(vm, fullServer())

	if vm.Status.Nova.LastSyncTime != ts {
		t.Error("applyNovaState must preserve LastSyncTime (Reconcile owns it)")
	}
}

func TestApplyNovaState_nilServerClears(t *testing.T) {
	vm := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	applyNovaState(vm, fullServer())
	synced := new(metav1.Now())
	vm.Status.Nova.LastSyncTime = synced

	applyNovaState(vm, nil)

	if vm.Spec.Nova.InstanceName != "" || vm.Spec.Nova.Flavor.Name != "" || vm.Status.Host != nil {
		t.Errorf("expected nova fields cleared, got spec=%+v host=%+v", vm.Spec.Nova, vm.Status.Host)
	}
	if novaFieldsSet(vm) {
		t.Error("novaFieldsSet should be false after clearing")
	}
	// LastSyncTime survives clearing so the orphan TTL can be measured from it.
	if vm.Status.Nova == nil || vm.Status.Nova.LastSyncTime != synced {
		t.Error("clear must preserve LastSyncTime")
	}
}

func TestNovaStateDiffers(t *testing.T) {
	server := &servers.Server{ID: "a", Status: "ACTIVE", Host: "compute-1"}

	inSync := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	applyNovaState(inSync, server)
	if novaStateDiffers(inSync, server) {
		t.Error("a freshly-applied VM must not differ from its server")
	}

	// LastSyncTime must not count as a difference.
	inSync.Status.Nova.LastSyncTime = new(metav1.Now())
	if novaStateDiffers(inSync, server) {
		t.Error("LastSyncTime must not make a VM look out of sync")
	}

	changed := inSync.DeepCopy()
	if !novaStateDiffers(changed, &servers.Server{ID: "a", Status: "SHUTOFF", Host: "compute-1"}) {
		t.Error("a status change must be detected as a difference")
	}
}

func TestNovaFieldsSet(t *testing.T) {
	empty := &v1alpha1.VirtualMachine{ObjectMeta: metav1.ObjectMeta{Name: "a"}}
	if novaFieldsSet(empty) {
		t.Error("a bare VM has no nova fields set")
	}
	populated := empty.DeepCopy()
	applyNovaState(populated, &servers.Server{ID: "a", Status: "ACTIVE"})
	if !novaFieldsSet(populated) {
		t.Error("a populated VM has nova fields set")
	}
	// A VM that only carries LastSyncTime counts as cleared.
	cleared := empty.DeepCopy()
	cleared.Status.Nova = &v1alpha1.NovaStatus{LastSyncTime: new(metav1.Now())}
	if novaFieldsSet(cleared) {
		t.Error("a VM carrying only LastSyncTime should not count as having nova fields")
	}
}

func TestFlavorImageFromServer_missingKeys(t *testing.T) {
	// Booted-from-volume / minimal server: empty maps must not panic.
	server := &servers.Server{ID: "a", Status: "ACTIVE"}
	if f := flavorFromServer(server); f.Name != "" || f.FlavorID != "" || f.ExtraSpecs != nil {
		t.Errorf("expected empty flavor, got %+v", f)
	}
	if img := imageFromServer(server); img.UUID != "" {
		t.Errorf("expected empty image, got %+v", img)
	}
}
