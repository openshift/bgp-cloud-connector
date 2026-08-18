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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ConditionCUDNCreated                = "CUDNCreated"
	ConditionRouteAdvertisementsCreated = "RouteAdvertisementsCreated"
)

// +kubebuilder:validation:XValidation:rule="self.subnets.all(s, isCIDR(s) && cidr(s).prefixLength() <= 30)",message="each subnet must be a valid CIDR with prefix /30 or wider"
type NetworkConfig struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	Subnets []string `json:"subnets"`
}

type CUDNBgpRoutingSpec struct {
	Network NetworkConfig `json:"network"`
}

type CUDNBgpRoutingStatus struct {
	Phase              PhaseType          `json:"phase,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Network",type="string",JSONPath=".spec.network.name"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CUDNBgpRouting declares a single CUDN network to advertise via BGP.
// Users must pre-create and label namespaces; the operator manages only the CUDN and RouteAdvertisements.
type CUDNBgpRouting struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CUDNBgpRoutingSpec   `json:"spec,omitempty"`
	Status CUDNBgpRoutingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type CUDNBgpRoutingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CUDNBgpRouting `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CUDNBgpRouting{}, &CUDNBgpRoutingList{})
}
