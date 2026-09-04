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
)

const (
	ConditionNetworkCreated             = "NetworkCreated"
	ConditionRouteAdvertisementsCreated = "RouteAdvertisementsCreated"
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
type BGPRoutingSpec struct {
	// Network defines the network to create and advertise.
	Network NetworkConfig `json:"network"`
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
	// ObservedGeneration is the most recent generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=bgpr,categories=networking
// +kubebuilder:printcolumn:name="Network",type="string",JSONPath=".spec.network.name"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +operator-sdk:csv:customresourcedefinitions:displayName="BGP Routing"

// BGPRouting declares a single network to advertise via BGP.
// Users must pre-create and label namespaces; the operator manages only the
// ClusterUserDefinedNetwork and RouteAdvertisements.
type BGPRouting struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BGPRoutingSpec   `json:"spec,omitempty"`
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
	SchemeBuilder.Register(&BGPRouting{}, &BGPRoutingList{})
}
