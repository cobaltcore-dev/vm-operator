// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package nova

// OpenStackClient abstracts the OpenStack Nova API as far as this controller
// needs it: it reports which Nova servers changed so the controller can
// translate them into VirtualMachine reconcile requests.
//
// The concrete implementation (talking to Nova over gophercloud or similar)
// will be added later; for now the controller depends only on this interface
// so it can be exercised with a stub in tests.
type OpenStackClient interface {
	// FetchChangedServers returns a list of server uuids
	// that has changed since the last call.
	FetchChangedServers() ([]string, error)
}
