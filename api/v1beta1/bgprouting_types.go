/*
Copyright 2026.

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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	ConditionNetworkCreated              = "NetworkCreated"
	ConditionNetworkResolved             = "NetworkResolved"
	ConditionRouteAdvertisementsAccepted = "RouteAdvertisementsAccepted"
	ConditionRouteAdvertisementsCreated  = "RouteAdvertisementsCreated"
)

// NetworkConfig defines a network to be created and advertised via BGP.
//
// +kubebuilder:validation:XValidation:rule="self.subnets.all(s, isCIDR(s))",message="each subnet must be a valid CIDR (e.g. 10.0.0.0/16 or 2001:db8::/64)"
type NetworkConfig struct {
	// Name identifies the network. The operator creates a ClusterUserDefinedNetwork
	// named cluster-udn-<name> that selects namespaces with label cluster-udn: <name>.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Subnets is the list of CIDRs for the network (1 for single-stack, 2 for dual-stack).
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +listType=atomic
	Subnets []string `json:"subnets"`
}

// BGPRoutingSpec defines the desired routing configuration for a single
// network to advertise via BGP.
// +kubebuilder:validation:XValidation:rule="has(self.network) != has(self.networkRef)",message="exactly one of network and networkRef must be set"
// +kubebuilder:validation:XValidation:rule="has(self.networkRef) == has(oldSelf.networkRef)",message="the network source is immutable"
type BGPRoutingSpec struct {
	// Network defines the network to create and advertise.
	// +optional
	Network *NetworkConfig `json:"network,omitempty"`
	// NetworkRef references an existing primary ClusterUserDefinedNetwork.
	// The network must opt in via the networking.openshift.io/bgp-network-ref
	// label with its UID as the value.
	// +optional
	NetworkRef *RoutingNetworkReference `json:"networkRef,omitempty"`
}

// RoutingNetworkReference identifies an administrator-managed cluster network.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="networkRef is immutable"
type RoutingNetworkReference struct {
	// Kind is the kind in the k8s.ovn.org API group.
	// +kubebuilder:validation:Enum=ClusterUserDefinedNetwork
	Kind string `json:"kind"`
	// Name is the name of the existing network.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// ResolvedRoutingNetwork describes the network observed by the controller.
type ResolvedRoutingNetwork struct {
	Name     string `json:"name"`
	UID      string `json:"uid"`
	Role     string `json:"role"`
	Topology string `json:"topology"`
}

// RoutingAdvertisementReference identifies the generated RouteAdvertisements.
type RoutingAdvertisementReference struct {
	Name string `json:"name"`
}

// BGPRoutingStatus defines the observed state of BGPRouting.
type BGPRoutingStatus struct {
	// Phase is the current lifecycle phase of the routing configuration.
	// +optional
	Phase PhaseType `json:"phase,omitempty"`
	// Conditions represent the latest available observations of the resource's state.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// NetworkRef identifies the CUDN that was provisioned or resolved.
	// +optional
	NetworkRef *ResolvedRoutingNetwork `json:"networkRef,omitempty"`
	// RouteAdvertisementsRef identifies the dedicated or shared advertisement.
	// +optional
	RouteAdvertisementsRef *RoutingAdvertisementReference `json:"routeAdvertisementsRef,omitempty"`
	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=bgpr,categories=networking
// +kubebuilder:printcolumn:name="Network",type="string",JSONPath=".spec.network.name"
// +kubebuilder:printcolumn:name="Network Ref",type="string",JSONPath=".spec.networkRef.name"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Available",type="string",JSONPath=".status.conditions[?(@.type=='Available')].status"
// +kubebuilder:printcolumn:name="Reason",type="string",JSONPath=".status.conditions[?(@.type=='Available')].reason"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:printcolumn:name="Message",type="string",priority=1,JSONPath=".status.conditions[?(@.type=='Available')].message"
// +operator-sdk:csv:customresourcedefinitions:displayName="BGP Routing"

// BGPRouting declares a single network to advertise via BGP.
// It can provision a ClusterUserDefinedNetwork or advertise an existing one.
type BGPRouting struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BGPRoutingSpec   `json:"spec"`
	Status BGPRoutingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// BGPRoutingList contains a list of BGPRouting.
type BGPRoutingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BGPRouting `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &BGPRouting{}, &BGPRoutingList{})
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})
}
