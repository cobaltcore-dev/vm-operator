// Copyright SAP SE
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ResourceName identifies a VM resource type.
type ResourceName string

const (
	// ResourceCPU is the number of virtual CPUs, in cores.
	ResourceCPU ResourceName = "cpu"
	// ResourceMemory is the amount of RAM, in bytes (e.g. "8Gi").
	ResourceMemory ResourceName = "memory"
)

// Label keys set on VirtualMachine objects.
const (
	// LabelRegion is the cloud region. Immutable.
	LabelRegion = "cobaltcore.cloud/region"
	// LabelAZ is the availability zone. Immutable.
	LabelAZ = "cobaltcore.cloud/az"

	// LabelTargetHost is the host where the VM should be placed on (spec).
	LabelTargetHost = "cobaltcore.cloud/target-host"
	// LabelHost is the host the VM is currently observed on (status).
	LabelHost = "cobaltcore.cloud/host"

	// LabelTargetCluster is the cluster where the VM should be placed in (spec).
	LabelTargetCluster = "cobaltcore.cloud/target-cluster"
	// LabelCluster is the cluster the VM is currently observed in (status).
	LabelCluster = "cobaltcore.cloud/cluster"
)

// VMConditionType identifies a condition on a VirtualMachine.
type VMConditionType = string

const (
	// ConditionTypeScheduled is set by Cortex after each scheduling operation.
	// status=Unknown: scheduling in-flight.
	// status=True: scheduling succeeded, one or multiple candidates found.
	// status=False: scheduling failed, see reason and message.
	ConditionTypeScheduled VMConditionType = "Scheduled"

	// ConditionTypeRunning is set by the VM operator when the VM domain is executing on a hypervisor.
	// status=True: domain running, including during live migration or resize.
	// status=False: domain paused, shut off, crashed, or not found.
	// status=Unknown: exporter cannot reach the host.
	ConditionTypeRunning VMConditionType = "Running"

	// ConditionTypeMigrating is set by the VM operator during a live migration.
	// status=True: migration in progress or aborting.
	// status=False: migration completed, rolled back, or failed.
	// status=Unknown: VM visible on multiple hosts, state unclear.
	ConditionTypeMigrating VMConditionType = "Migrating"

	// ConditionTypeResizing is set by the VM operator during a resize operation.
	// status=True: resize in progress or being reverted.
	// status=False: resize completed, reverted, or failed.
	// status=Unknown: resize state unclear.
	ConditionTypeResizing VMConditionType = "Resizing"

	// ConditionTypeNovaInSync is set by the VM operator to record if the libvirt-observed state is consistent with what Nova reports.
	// status=True: host and state consistent.
	// status=False: mismatch detected — see reason and message.
	// status=Unknown: not yet checked.
	ConditionTypeNovaInSync VMConditionType = "NovaInSync"
)

// Reasons for ConditionTypeRunning.
const (
	ConditionReasonRunningShutoff  = "Shutoff"
	ConditionReasonRunningCrashed  = "Crashed"
	ConditionReasonRunningPaused   = "Paused"
	ConditionReasonRunningNotFound = "NotFound"
)

// Reasons for ConditionTypeMigrating.
const (
	ConditionReasonMigratingOngoing    = "Ongoing"
	ConditionReasonMigratingAborting   = "Aborting"
	ConditionReasonMigratingCompleted  = "Completed"
	ConditionReasonMigratingRolledBack = "RolledBack"
	ConditionReasonMigratingFailed     = "Failed"
)

// Reasons for ConditionTypeResizing.
const (
	ConditionReasonResizingOngoing   = "Ongoing"
	ConditionReasonResizingReverting = "Reverting"
	ConditionReasonResizingCompleted = "Completed"
	ConditionReasonResizingReverted  = "Reverted"
	ConditionReasonResizingFailed    = "Failed"
)

// Reasons for ConditionTypeNovaInSync.
const (
	ConditionReasonNovaInSyncHostMismatch  = "HostMismatch"
	ConditionReasonNovaInSyncStateMismatch = "StateMismatch"
	ConditionReasonNovaInSyncUnreachable   = "NovaUnreachable"
)

// Reasons for ConditionTypeScheduled.
const (
	// Operation type reasons — set on both success and failure of a scheduling call.
	ConditionReasonScheduledInitialPlacement = "InitialPlacement"
	ConditionReasonScheduledLiveMigration    = "LiveMigration"
	ConditionReasonScheduledEvacuation       = "Evacuation"
	ConditionReasonScheduledResize           = "Resize"

	// Failure reasons — set on status=False.
	ConditionReasonScheduledNoHostFound = "NoHostFound"
	ConditionReasonScheduledTimeout     = "Timeout"
	ConditionReasonScheduledError       = "Error"
)

// VMSpec defines the desired state of a VirtualMachine.
// Written by the Cortex placement API at scheduling time and updated on lifecycle events.
type VMSpec struct {
	// Region is the cloud region this VM is placed in.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="region is immutable"
	Region string `json:"region"`

	// AZ is the Nova availability zone the VM was scheduled into.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="az is immutable"
	AZ string `json:"az"`

	// TargetHost is the compute host ref of the hypervisor where this VM should be placed.
	// +kubebuilder:validation:Optional
	TargetHost *HostRef `json:"targetHost,omitempty"`

	// NovaSpec is the last known spec of this VM inside the OpenStack Nova service.
	// +kubebuilder:validation:Required
	Nova NovaSpec `json:"nova"`

	// Cortex captures the desired state of this VM in Cortex.
	// +kubebuilder:validation:Optional
	Cortex *CortexSpec `json:"cortex,omitempty"`
}

// NovaSpec captures the Nova instance metadata and scheduling request for this VM.
type NovaSpec struct {
	// InstanceName is the human-readable Nova instance name, used for logging and debugging only.
	// +kubebuilder:validation:Optional
	InstanceName string `json:"instanceName,omitempty"`

	// CreatedAt is the Nova instance creation timestamp.
	// +kubebuilder:validation:Optional
	CreatedAt *metav1.Time `json:"createdAt,omitempty"`

	// Resources describes the compute resources requested by the VM.
	// Keys are ResourceName constants (cpu, memory); values are Kubernetes resource quantities.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="\"cpu\" in self && \"memory\" in self",message="both cpu and memory must be specified"
	// +kubebuilder:validation:XValidation:rule="self.all(k, quantity(self[k]).isGreaterThan(quantity(\"0\")))",message="all resources must be greater than 0"
	Resources map[ResourceName]resource.Quantity `json:"resources"`

	// Ownership identifies the OpenStack project and user owning this VM.
	// +kubebuilder:validation:Required
	Ownership Ownership `json:"ownership"`

	// Flavor is the Nova flavor used for this VM.
	// +kubebuilder:validation:Required
	Flavor Flavor `json:"flavor"`

	// Image is the Glance image used to boot the VM.
	// +kubebuilder:validation:Required
	Image Image `json:"image"`

	// InstanceGroup holds anti/affinity group membership for this VM.
	// Used by affinity/anti-affinity filters to enforce constraints across concurrent placements.
	// +kubebuilder:validation:Optional
	InstanceGroup *InstanceGroup `json:"instanceGroup,omitempty"`
}

// Ownership identifies the OpenStack project and user that own a VM.
type Ownership struct {
	// Project is the OpenStack project (tenant) UUID.
	// +kubebuilder:validation:Required
	Project string `json:"project"`

	// User is the OpenStack user UUID that created the VM.
	// +kubebuilder:validation:Optional
	User string `json:"user,omitempty"`
}

// Flavor describes the Nova flavor of a VM.
type Flavor struct {
	// Name is the Nova flavor name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// FlavorID is the Nova flavor id.
	// +kubebuilder:validation:Optional
	FlavorID string `json:"flavorID,omitempty"`

	// ExtraSpecs holds flavor extra specs, e.g. hypervisor type, custom traits, hardware requirements.
	// +kubebuilder:validation:Optional
	ExtraSpecs map[string]string `json:"extraSpecs,omitempty"`
}

// Image describes the Glance image used to boot a VM.
type Image struct {
	// UUID is the Glance image UUID.
	// +kubebuilder:validation:Required
	UUID string `json:"uuid"`

	// Properties holds arbitrary image properties from Glance (e.g. os_type, hw_machine_type).
	// +kubebuilder:validation:Optional
	Properties map[string]string `json:"properties,omitempty"`
}

// InstanceGroup holds Nova instance group membership for a VM.
type InstanceGroup struct {
	// UUID is the Nova instance group UUID.
	// +kubebuilder:validation:Required
	UUID string `json:"uuid"`

	// Policy is the scheduling policy, e.g. "anti-affinity", "soft-anti-affinity".
	// +kubebuilder:validation:Required
	Policy string `json:"policy"`
}

// CortexSpec captures the desired state of this VM in Cortex.
type CortexSpec struct {
	// HostCandidates is the set of hosts Cortex has considered for the most recent scheduling operation of the VM.
	// +kubebuilder:validation:Optional
	HostCandidates []HostRef `json:"hostCandidates,omitempty"`
}

// HostRef is a reference to a hypervisor host.
type HostRef struct {
	// Name is the compute host name.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Cluster is the compute cluster this host belongs to.
	// +kubebuilder:validation:Required
	Cluster string `json:"cluster"`
}

// VMStatus defines the observed state of a VirtualMachine.
type VMStatus struct {
	// Host is the compute host this VM is currently running on.
	// +kubebuilder:validation:Optional
	Host *HostRef `json:"host,omitempty"`

	// Nova holds the last known state of this VM as reported by Nova.
	// +kubebuilder:validation:Optional
	Nova *NovaStatus `json:"nova,omitempty"`

	// Libvirt holds state reported by libvirt for this VM.
	// +kubebuilder:validation:Optional
	Libvirt *LibvirtStatus `json:"libvirt,omitempty"`

	// Conditions holds standard Kubernetes conditions for this VM.
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:Optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NovaStatus holds the last known Nova instance state for a VM.
type NovaStatus struct {
	// Status is the Nova instance status (e.g. ACTIVE, SHUTOFF, MIGRATING, ERROR, BUILD).
	// +kubebuilder:validation:Optional
	Status string `json:"status,omitempty"`

	// LastSyncTime is the timestamp of the last successful sync from Nova.
	// +kubebuilder:validation:Optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
}

// LibvirtStatus holds state reported by libvirt for a VM.
// ObservedHosts is populated when the VM is visible on multiple hosts, e.g. during live migration.
type LibvirtStatus struct {
	// ObservedHosts maps hypervisor hostnames to their observed VM state.
	// +kubebuilder:validation:Optional
	ObservedHosts map[string]LibvirtHostInfo `json:"observedHosts,omitempty"`
}

// LibvirtHostInfo holds the libvirt-reported state of a VM on a specific host.
type LibvirtHostInfo struct{}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=vm
// +kubebuilder:selectablefield:JSONPath=".spec.region"
// +kubebuilder:selectablefield:JSONPath=".spec.az"
// +kubebuilder:selectablefield:JSONPath=".spec.targetHost.name"
// +kubebuilder:selectablefield:JSONPath=".spec.targetHost.cluster"
// +kubebuilder:selectablefield:JSONPath=".status.host.name"
// +kubebuilder:selectablefield:JSONPath=".status.host.cluster"
// +kubebuilder:printcolumn:JSONPath=".spec.az",name="AZ",type="string"
// +kubebuilder:printcolumn:JSONPath=".spec.nova.flavor.name",name="Flavor",type="string"
// +kubebuilder:printcolumn:JSONPath=".spec.nova.resources.cpu",name="CPU",type="string"
// +kubebuilder:printcolumn:JSONPath=".spec.nova.resources.memory",name="Memory",type="string"
// +kubebuilder:printcolumn:JSONPath=".status.host.name",name="Host",type="string"
// +kubebuilder:printcolumn:JSONPath=".status.conditions[?(@.type==\"Running\")].status",name="Running",type="string"
// +kubebuilder:printcolumn:JSONPath=".status.conditions[?(@.type==\"Running\")].reason",name="State",type="string"
// +kubebuilder:printcolumn:JSONPath=".status.conditions[?(@.type==\"NovaInSync\")].status",name="NovaInSync",type="string"
// +kubebuilder:printcolumn:JSONPath=".status.nova.status",name="NovaState",type="string"
// +kubebuilder:printcolumn:JSONPath=".metadata.creationTimestamp",name="Age",type="date"
// +kubebuilder:printcolumn:JSONPath=".spec.nova.ownership.project",name="Project",type="string",priority=1
// +kubebuilder:printcolumn:JSONPath=".status.conditions[?(@.type==\"Migrating\")].status",name="Migrating",type="string",priority=1
// +kubebuilder:printcolumn:JSONPath=".status.conditions[?(@.type==\"Resizing\")].status",name="Resizing",type="string",priority=1
// +kubebuilder:printcolumn:JSONPath=".status.conditions[?(@.type==\"Scheduled\")].reason",name="Scheduled",type="string",priority=1
// +kubebuilder:printcolumn:JSONPath=".status.conditions[?(@.type==\"NovaInSync\")].reason",name="NovaInSyncReason",type="string",priority=1
// +kubebuilder:printcolumn:JSONPath=".status.nova.lastSyncTime",name="NovaLastSync",type="date",priority=1

// VirtualMachine tracks a Nova VM from initial placement through its full lifetime.
type VirtualMachine struct {
	metav1.TypeMeta `json:",inline"`
	// metadata.name holds the Nova instance UUID (e.g. "63ed64e2-27a2-4b70-9b5c-833c2a285d66").
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VMSpec   `json:"spec,omitempty"`
	Status VMStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VirtualMachineList contains a list of VirtualMachine.
type VirtualMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(scheme *runtime.Scheme) error {
		scheme.AddKnownTypes(GroupVersion, &VirtualMachine{}, &VirtualMachineList{})
		return nil
	})
}
