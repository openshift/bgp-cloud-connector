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

// +kubebuilder:validation:Enum=bfd;"bgp-keepalive"
type LivenessDetectionType string

const (
	LivenessDetectionBFD          LivenessDetectionType = "bfd"
	LivenessDetectionBGPKeepalive LivenessDetectionType = "bgp-keepalive"
)

// +kubebuilder:validation:Enum=Pending;Configuring;Ready;Degraded
type PhaseType string

const (
	PhasePending     PhaseType = "Pending"
	PhaseConfiguring PhaseType = "Configuring"
	PhaseReady       PhaseType = "Ready"
	PhaseDegraded    PhaseType = "Degraded"
)

const (
	ConditionNetworkOperatorPatched  = "NetworkOperatorPatched"
	ConditionFRRNamespaceReady       = "FRRNamespaceReady"
	ConditionAWSEndpointsDiscovered  = "AWSEndpointsDiscovered"
	ConditionFRRConfigurationApplied = "FRRConfigurationApplied"
	ConditionAWSResourcesReconciled  = "AWSResourcesReconciled"
	ConditionIncompleteNodeInventory = "IncompleteNodeInventory"
)

type BGPNeighbor struct {
	Address string `json:"address"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	RemoteASN int64 `json:"remoteASN"`
}

type AvailabilityZone struct {
	NodeSelector map[string]string `json:"nodeSelector"`
	// +kubebuilder:validation:MinItems=1
	Neighbors []BGPNeighbor `json:"neighbors"`
}

type AWSConfig struct {
	Region string `json:"region"`
	// +kubebuilder:validation:MinItems=1
	RouteServerIDs []string `json:"routeServerIDs"`
}

type DiscoveredEndpoint struct {
	EndpointID       string `json:"endpointID"`
	AvailabilityZone string `json:"availabilityZone"`
	Address          string `json:"address"`
}

type DiscoveredRouteServer struct {
	RouteServerID string               `json:"routeServerID"`
	RemoteASN     int64                `json:"remoteASN"`
	Endpoints     []DiscoveredEndpoint `json:"endpoints,omitempty"`
}

type AWSStatus struct {
	RouteServers []DiscoveredRouteServer `json:"routeServers,omitempty"`
}

type BGPConfig struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	LocalASN int64 `json:"localASN"`
	// +kubebuilder:default="bgp-keepalive"
	LivenessDetection LivenessDetectionType `json:"livenessDetection,omitempty"`
	// +optional
	AvailabilityZones []AvailabilityZone `json:"availabilityZones,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!(has(self.aws) && has(self.bgp.availabilityZones) && size(self.bgp.availabilityZones) > 0)",message="spec.aws and spec.bgp.availabilityZones are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="has(self.aws) || (has(self.bgp.availabilityZones) && size(self.bgp.availabilityZones) > 0)",message="spec.bgp.availabilityZones is required when spec.aws is not configured"
type CUDNBgpConfigSpec struct {
	BGP                BGPConfig         `json:"bgp"`
	RouterNodeSelector map[string]string `json:"routerNodeSelector"`
	AWS                *AWSConfig        `json:"aws,omitempty"`
}

type CUDNBgpConfigStatus struct {
	Phase              PhaseType          `json:"phase,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	AWS                *AWSStatus         `json:"aws,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// CUDNBgpConfig is the singleton cluster-scoped BGP infrastructure configuration.
type CUDNBgpConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CUDNBgpConfigSpec   `json:"spec,omitempty"`
	Status CUDNBgpConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type CUDNBgpConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CUDNBgpConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CUDNBgpConfig{}, &CUDNBgpConfigList{})
}
