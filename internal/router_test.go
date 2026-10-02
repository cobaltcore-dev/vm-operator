// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package internal

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

func vmWithAZ(specAZ, labelAZ string) *v1alpha1.VirtualMachine {
	vm := &v1alpha1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vm"},
		Spec:       v1alpha1.VMSpec{AZ: specAZ},
	}
	if labelAZ != "" {
		vm.Labels = map[string]string{v1alpha1.LabelAZ: labelAZ}
	}
	return vm
}

func TestVirtualMachineRouter_Match(t *testing.T) {
	tests := []struct {
		name    string
		obj     any
		labels  map[string]string
		want    bool
		wantErr bool
	}{
		{
			name:   "spec AZ matches cluster",
			obj:    vmWithAZ("qa-de-1a", ""),
			labels: map[string]string{clusterLabelAvailabilityZone: "qa-de-1a"},
			want:   true,
		},
		{
			name:   "spec AZ does not match cluster",
			obj:    vmWithAZ("qa-de-1a", ""),
			labels: map[string]string{clusterLabelAvailabilityZone: "qa-de-1b"},
			want:   false,
		},
		{
			name:   "falls back to label AZ when spec empty",
			obj:    vmWithAZ("", "qa-de-1a"),
			labels: map[string]string{clusterLabelAvailabilityZone: "qa-de-1a"},
			want:   true,
		},
		{
			name:   "value form is accepted",
			obj:    *vmWithAZ("qa-de-1a", ""),
			labels: map[string]string{clusterLabelAvailabilityZone: "qa-de-1a"},
			want:   true,
		},
		{
			name:    "cluster missing availabilityZone label",
			obj:     vmWithAZ("qa-de-1a", ""),
			labels:  map[string]string{},
			wantErr: true,
		},
		{
			name:    "VM has no availability zone",
			obj:     vmWithAZ("", ""),
			labels:  map[string]string{clusterLabelAvailabilityZone: "qa-de-1a"},
			wantErr: true,
		},
		{
			name:    "wrong object type",
			obj:     "not-a-vm",
			labels:  map[string]string{clusterLabelAvailabilityZone: "qa-de-1a"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := VirtualMachineRouter{}.Match(tc.obj, tc.labels)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (result %v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVirtualMachineRouter_ExtractClusterSelector(t *testing.T) {
	got, err := VirtualMachineRouter{}.ExtractClusterSelector(vmWithAZ("qa-de-1a", ""))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "qa-de-1a" {
		t.Errorf("ExtractClusterSelector = %q, want %q", got, "qa-de-1a")
	}

	if _, err := (VirtualMachineRouter{}).ExtractClusterSelector(vmWithAZ("", "")); err == nil {
		t.Error("expected error for VM without availability zone, got nil")
	}
}
