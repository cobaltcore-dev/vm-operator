// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package internal

import (
	"errors"

	"github.com/cobaltcore-dev/cortex/pkg/multicluster"

	v1alpha1 "github.com/cobaltcore-dev/vm-operator/api/v1alpha1"
)

// clusterLabelAvailabilityZone is the label key a remote cluster carries (in
// its multicluster config) to declare which availability zone it serves.
const clusterLabelAvailabilityZone = "availabilityZone"

// VirtualMachineRouter routes VirtualMachine writes to the cluster whose
// "availabilityZone" label matches the VM's availability zone.
//
// It implements multicluster.ResourceRouter.
type VirtualMachineRouter struct{}

var _ multicluster.ResourceRouter = VirtualMachineRouter{}

// availabilityZone returns the VM's availability zone, preferring the spec.az
// field and falling back to the LabelAZ label.
func availabilityZone(vm *v1alpha1.VirtualMachine) string {
	if vm.Spec.AZ != "" {
		return vm.Spec.AZ
	}
	return vm.Labels[v1alpha1.LabelAZ]
}

// asVirtualMachine normalizes the pointer and value forms of a VirtualMachine.
func asVirtualMachine(obj any) (*v1alpha1.VirtualMachine, error) {
	switch v := obj.(type) {
	case *v1alpha1.VirtualMachine:
		if v == nil {
			return nil, errors.New("object is nil")
		}
		return v, nil
	case v1alpha1.VirtualMachine:
		return &v, nil
	default:
		return nil, errors.New("object is not a VirtualMachine")
	}
}

// Match reports whether the VM belongs to the cluster described by labels.
func (VirtualMachineRouter) Match(obj any, labels map[string]string) (bool, error) {
	vm, err := asVirtualMachine(obj)
	if err != nil {
		return false, err
	}
	clusterAZ, ok := labels[clusterLabelAvailabilityZone]
	if !ok {
		return false, errors.New("cluster does not have availabilityZone label")
	}
	vmAZ := availabilityZone(vm)
	if vmAZ == "" {
		return false, errors.New("virtual machine does not have an availability zone")
	}
	return vmAZ == clusterAZ, nil
}

// ExtractClusterSelector returns the VM's availability zone, used to enrich
// error messages when no cluster matches.
func (VirtualMachineRouter) ExtractClusterSelector(obj any) (string, error) {
	vm, err := asVirtualMachine(obj)
	if err != nil {
		return "", err
	}
	vmAZ := availabilityZone(vm)
	if vmAZ == "" {
		return "", errors.New("virtual machine does not have an availability zone")
	}
	return vmAZ, nil
}
