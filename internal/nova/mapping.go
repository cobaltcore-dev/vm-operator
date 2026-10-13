// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

import (
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

// hypervisorTypeExtraSpec is the flavor extra spec that records which hypervisor
// a flavor targets. Only "qemu" and "ch" are managed by this operator.
const hypervisorTypeExtraSpec = "capabilities:hypervisor_type"

// hypervisorTypeFromFlavor maps a flavor's capabilities:hypervisor_type extra
// spec onto a VMSpec HypervisorType. ok is false when the flavor is nil, lacks
// the extra spec, or carries an unsupported value — such servers are not managed
// by this operator and must be skipped.
func hypervisorTypeFromFlavor(flavor *flavors.Flavor) (v1alpha1.HypervisorType, bool) {
	if flavor == nil {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(flavor.ExtraSpecs[hypervisorTypeExtraSpec])) {
	case "qemu":
		return v1alpha1.HypervisorTypeQEMU, true
	case "ch":
		return v1alpha1.HypervisorTypeCH, true
	default:
		return "", false
	}
}

// This file is the single place that knows how a Nova server maps onto a
// VirtualMachine resource. Both the poll loop (to decide whether a reconcile is
// needed) and Reconcile (to perform the sync) go through these helpers, so the
// definition of "the Nova-derived fields" lives in exactly one spot.
//
// LastSyncTime is intentionally NOT part of the mapping. It is a write-time
// side effect stamped by Reconcile, excluded from every comparison so it never
// by itself makes a resource look out of sync (which would churn forever).

// applyNovaState overlays Nova's view of a server onto vm in place. A nil
// server means Nova no longer knows the server, in which case the Nova-derived
// fields are cleared rather than populated.
//
// It touches only the fields this controller owns:
//   - Spec.Nova: InstanceName, CreatedAt, Ownership, Flavor, Image
//   - Status.Nova.Status
//
// Status.Nova.LastSyncTime is preserved (Reconcile owns it); conditions are
// left untouched. Status.Host is NOT owned here: it reflects the host the VM is
// actually running on, which the operator derives from libvirt/cloud-hypervisor
// elsewhere, not from Nova's placement view.
func applyNovaState(vm *v1alpha1.VirtualMachine, server *servers.Server) {
	// LastSyncTime survives both populate and clear so the cleanup controller
	// can measure the orphan TTL from it.
	var lastSync *metav1.Time
	if vm.Status.Nova != nil {
		lastSync = vm.Status.Nova.LastSyncTime
	}

	if server == nil {
		vm.Spec.Nova = v1alpha1.NovaSpec{}
		vm.Status.Nova = &v1alpha1.NovaStatus{LastSyncTime: lastSync}
		return
	}

	vm.Spec.Nova = v1alpha1.NovaSpec{
		InstanceName: server.InstanceName,
		CreatedAt:    nonZeroTime(server.Created),
		Ownership:    v1alpha1.Ownership{Project: server.TenantID, User: server.UserID},
		Flavor:       flavorFromServer(server),
		Image:        imageFromServer(server),
	}
	vm.Status.Nova = &v1alpha1.NovaStatus{Status: server.Status, LastSyncTime: lastSync}
}

// novaStateDiffers reports whether applying server to vm would change any
// Nova-derived field, i.e. the resource is not yet in sync. The poll loop uses
// it to decide whether to enqueue, without issuing a per-server Nova call.
func novaStateDiffers(vm *v1alpha1.VirtualMachine, server *servers.Server) bool {
	updated := vm.DeepCopy()
	applyNovaState(updated, server)
	return !novaStateEqual(vm, updated)
}

// novaFieldsSet reports whether vm still carries any Nova-derived field. It lets
// the poll and cleanup loops tell apart a resource that already reflects "no
// server" from one still awaiting that clear.
func novaFieldsSet(vm *v1alpha1.VirtualMachine) bool {
	cleared := vm.DeepCopy()
	applyNovaState(cleared, nil)
	return !novaStateEqual(vm, cleared)
}

// novaStateEqual reports whether a and b carry the same Nova-derived state,
// ignoring LastSyncTime (a write-time side effect) and conditions (managed
// separately). It is the one comparison the poll decisions are built on.
func novaStateEqual(a, b *v1alpha1.VirtualMachine) bool {
	return equality.Semantic.DeepEqual(a.Spec.Nova, b.Spec.Nova) &&
		novaStatus(a) == novaStatus(b)
}

// novaStatus returns the observed Nova status string, treating a nil
// Status.Nova as empty so a never-synced resource compares cleanly.
func novaStatus(vm *v1alpha1.VirtualMachine) string {
	if vm.Status.Nova == nil {
		return ""
	}
	return vm.Status.Nova.Status
}

// specChanged reports whether a and b have different specs, so the sync path
// knows whether a spec patch (distinct from a status patch) is needed.
func specChanged(a, b *v1alpha1.VirtualMachine) bool {
	return !equality.Semantic.DeepEqual(a.Spec, b.Spec)
}

// flavorFromServer extracts the VM flavor from a Nova server's embedded flavor
// object. gophercloud exposes it as a loosely-typed map; this reads the keys
// Nova populates in the server detail response.
func flavorFromServer(server *servers.Server) v1alpha1.Flavor {
	flavor := v1alpha1.Flavor{
		Name:     stringField(server.Flavor, "original_name"),
		FlavorID: stringField(server.Flavor, "id"),
	}
	if specs := stringMapField(server.Flavor, "extra_specs"); len(specs) > 0 {
		flavor.ExtraSpecs = specs
	}
	return flavor
}

// imageFromServer extracts the Glance image reference from a Nova server's
// embedded image object. Booted-from-volume servers report an empty image.
func imageFromServer(server *servers.Server) v1alpha1.Image {
	return v1alpha1.Image{UUID: stringField(server.Image, "id")}
}

// hostRef builds a HostRef for a host name, or nil when the server reports none.
func hostRef(host string) *v1alpha1.HostRef {
	if host == "" {
		return nil
	}
	return &v1alpha1.HostRef{Name: host}
}

// stringField returns m[key] as a string, or "" if absent or not a string.
func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// stringMapField returns m[key] as a map[string]string, or nil if absent or not
// a string map. Nova renders extra_specs as a nested JSON object.
func stringMapField(m map[string]any, key string) map[string]string {
	nested, ok := m[key].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(nested))
	for k, v := range nested {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// nonZeroTime wraps a time as a *metav1.Time, returning nil for the zero time so
// an unset Nova timestamp does not become an epoch value on the resource.
func nonZeroTime(t time.Time) *metav1.Time {
	if t.IsZero() {
		return nil
	}
	return new(metav1.NewTime(t))
}
