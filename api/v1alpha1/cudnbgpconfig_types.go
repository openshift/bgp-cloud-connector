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

// LivenessDetectionType specifies how BGP peer health is monitored.
// +kubebuilder:validation:Enum=bfd;"bgp-keepalive"
type LivenessDetectionType string

const (
	LivenessDetectionBFD          LivenessDetectionType = "bfd"
	LivenessDetectionBGPKeepalive LivenessDetectionType = "bgp-keepalive"
)

// PhaseType represents the lifecycle phase of a resource.
// +kubebuilder:validation:Enum=Pending;Configuring;Ready;Degraded
type PhaseType string

const (
	PhasePending     PhaseType = "Pending"
	PhaseConfiguring PhaseType = "Configuring"
	PhaseReady       PhaseType = "Ready"
	PhaseDegraded    PhaseType = "Degraded"
)

// PlatformType selects which cloud the operator reconciles BGP peering
// against. It is the discriminator for the cloud-specific block in the spec.
// +kubebuilder:validation:Enum=AWS;Azure;GCP;Manual
type PlatformType string

// AllPlatforms is every value the enum above accepts. The dispatch test walks
// it, so a value added to the marker and forgotten here, or added to both and
// never given a builder, fails rather than surfacing as "no platform
// implementation" at runtime on a live cluster.
var AllPlatforms = []PlatformType{PlatformAWS, PlatformAzure, PlatformGCP, PlatformManual}

const (
	// PlatformAWS discovers BGP neighbours from VPC Route Server endpoints and
	// reconciles Route Server peers and source/dest check. Requires spec.aws.
	PlatformAWS PlatformType = "AWS"
	// PlatformAzure discovers BGP neighbours from an Azure Route Server and
	// reconciles its BGP connections and the router nodes' network interfaces.
	// Requires spec.azure.
	PlatformAzure PlatformType = "Azure"
	// PlatformGCP discovers BGP neighbors from a Cloud Router and reconciles
	// NCC spokes, Cloud Router peers and GCE instance attributes. Requires
	// spec.gcp.
	PlatformGCP PlatformType = "GCP"
	// PlatformManual performs no cloud reconciliation. BGP neighbours are
	// taken from spec.bgp.peerGroups.
	PlatformManual PlatformType = "Manual"
)

const (
	ConditionNetworkOperatorPatched = "NetworkOperatorPatched"
	ConditionFRRNamespaceReady      = "FRRNamespaceReady"
	// The discovery and reconcile conditions are the operator's own rather
	// than any one provider's: every cloud reports both, and only the API
	// calls beneath them differ.
	ConditionCloudEndpointsDiscovered = "CloudEndpointsDiscovered"
	ConditionFRRConfigurationApplied  = "FRRConfigurationApplied"
	ConditionCloudResourcesReconciled = "CloudResourcesReconciled"
	ConditionCompleteNodeInventory    = "CompleteNodeInventory"
)

// BGPNeighbor identifies a single BGP peer by its IP address and AS number.
type BGPNeighbor struct {
	// Address is the IP address of the BGP neighbor.
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:MaxLength=45
	// +kubebuilder:validation:XValidation:rule="isIP(self)",message="must be a valid IP address"
	Address string `json:"address"`
	// RemoteASN is the autonomous system number of the BGP neighbor.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	RemoteASN int64 `json:"remoteASN"`
	// EBGPMultiHop allows the session to be established with a peer that is
	// not on this node's link.
	//
	// An Azure Route Server needs it, because it sits in its own subnet rather
	// than on the node's; an AWS Route Server endpoint does not. Declared here
	// rather than inferred, because whether a neighbour is on the link is a
	// property of the cloud's topology and not something this operator can
	// work out.
	// +optional
	EBGPMultiHop bool `json:"ebgpMultiHop,omitempty"`
}

// PeerGroup is a set of router nodes sharing a neighbour set, and becomes one
// FRRConfiguration.
//
// How many groups a cluster has is a property of its cloud. Endpoints that are
// per subnet give one group per availability zone, because a node peers with
// the ones in its own; endpoints presented once for a region give a single
// group covering every router node.
type PeerGroup struct {
	// NodeSelector is a set of labels used to select the router nodes
	// that belong to this peer group.
	NodeSelector map[string]string `json:"nodeSelector"`
	// Neighbors is the list of BGP peers that nodes in this group establish sessions with.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +listType=atomic
	Neighbors []BGPNeighbor `json:"neighbors"`
}

// AWSConfig names the VPC Route Servers to discover. A VPC can hold several
// and their endpoints are per subnet, which is why AWS is the cloud that
// produces more than one peer group.
type AWSConfig struct {
	// Region is the AWS region where the ROSA cluster and Route Servers are deployed.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`
	// RouteServerIDs is the list of VPC Route Server IDs used for auto-discovery
	// of BGP endpoints, neighbor IPs, availability zones, and remote ASN.
	// +kubebuilder:validation:MinItems=1
	// +listType=atomic
	RouteServerIDs []string `json:"routeServerIDs"`
}

// AzureConfig names the Route Server. Its addresses and ASN are read from it
// rather than declared: there is exactly one per virtual network, so there is
// nothing to enumerate, and Azure fixes the far-side ASN with no flag to
// change it.
type AzureConfig struct {
	// +kubebuilder:validation:MinLength=1
	SubscriptionID string `json:"subscriptionID"`
	// +kubebuilder:validation:MinLength=1
	ResourceGroup string `json:"resourceGroup"`
	// RouteServerName is the Azure Route Server whose BGP connections this
	// operator manages. Azure models it as a Virtual Hub, and its
	// virtualRouterIps become the BGP neighbours.
	// +kubebuilder:validation:MinLength=1
	RouteServerName string `json:"routeServerName"`
	// NetworkInterfaceClientID is the managed identity to use for network
	// interface calls, where that differs from the identity the operator
	// otherwise runs as.
	//
	// A router node's interface and the Route Server can sit in resource
	// groups reachable by different identities. On ARO the interfaces are in a
	// resource group the cluster does not own, and the identity with write
	// access there is the cluster's own machine-api identity, which cannot be
	// granted to this operator. Naming it here exchanges the operator's
	// projected service account token for that identity, for interface calls
	// only; Route Server calls continue to use the identity the operator runs
	// as.
	//
	// The identity named here must carry a federated credential trusting this
	// operator's service account. Unset means one identity reaches both, which
	// is the case wherever the cluster owns the resource group holding its own
	// interfaces.
	// +optional
	NetworkInterfaceClientID string `json:"networkInterfaceClientID,omitempty"`
}

// NCCConfig identifies the Network Connectivity Center hub the router nodes
// attach to as spokes. On GCP, membership is a precondition of peering rather
// than part of it: a VM cannot peer with a Cloud Router until it belongs to a
// router appliance spoke.
type NCCConfig struct {
	// +kubebuilder:validation:MinLength=1
	HubName string `json:"hubName"`
	// SpokePrefix names the spokes this operator manages. Spokes are numbered
	// from it, because a spoke holds a limited number of instances.
	// +kubebuilder:validation:MinLength=1
	SpokePrefix string `json:"spokePrefix"`
	// SiteToSiteDataTransfer enables NCC site-to-site data transfer on the
	// managed spokes.
	// +optional
	SiteToSiteDataTransfer bool `json:"siteToSiteDataTransfer,omitempty"`
}

// GCPConfig names the Cloud Router carrying the BGP, and the hub and spoke
// that make a node eligible to peer with it.
type GCPConfig struct {
	// +kubebuilder:validation:MinLength=1
	Project string `json:"project"`
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`
	// CloudRouterName is the Cloud Router the router nodes peer with; its
	// interface addresses become the BGP neighbors. It must not be the
	// installer's Cloud NAT router, which has no interfaces and carries the
	// cluster's egress.
	// +kubebuilder:validation:MinLength=1
	CloudRouterName string    `json:"cloudRouterName"`
	NCC             NCCConfig `json:"ncc"`
	// EnableNestedVirtualization turns on nested virtualization on the router
	// instances, which KubeVirt needs. Enabling it restarts the instance.
	// +optional
	// +kubebuilder:default=true
	EnableNestedVirtualization *bool `json:"enableNestedVirtualization,omitempty"`
}

// PeerGroupStatus reports one group of the peering plan the operator
// discovered, and therefore what FRR was configured to peer with.
//
// Every cloud populates it, which is the bar a shared status block has to
// clear: one shaped around a single provider's resources leaves every other
// cloud reconciling peerings and reporting nothing about them, and a sibling
// block per cloud is worse.
type PeerGroupStatus struct {
	// Key names the group in cloud-meaningful terms: an availability zone on
	// AWS, and whatever names the single regional endpoint elsewhere.
	Key string `json:"key"`
	// NodeSelector narrows spec.routerNodeSelector to this group. Empty means
	// every router node.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// Neighbors are the addresses the router nodes in this group peer with.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=8
	Neighbors []BGPNeighbor `json:"neighbors,omitempty"`
}

// BGPConfig holds the BGP speaker configuration and optional peer groups.
type BGPConfig struct {
	// LocalASN is the autonomous system number for the cluster's FRR routers.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=4294967295
	LocalASN int64 `json:"localASN"`
	// LivenessDetection selects the mechanism used to detect BGP peer failure.
	// BFD detects failure in ~1s; bgp-keepalive relies on the BGP hold timer (~90s).
	// +optional
	// +kubebuilder:default="bgp-keepalive"
	LivenessDetection LivenessDetectionType `json:"livenessDetection,omitempty"`
	// PeerGroups defines explicit BGP peer groups with neighbor addresses.
	// Required when platform is Manual; must not be set on other platforms
	// where peer groups are auto-discovered.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	PeerGroups []PeerGroup `json:"peerGroups,omitempty"`
}

// CUDNBgpConfigSpec defines the desired BGP infrastructure configuration for the
// cluster: which cloud platform to integrate with, which nodes act as BGP
// routers, and how BGP sessions are established.
//
// +kubebuilder:validation:XValidation:rule="(self.platform == 'AWS') == has(self.aws)",message="spec.aws must be set when spec.platform is AWS, and must be absent otherwise"
// +kubebuilder:validation:XValidation:rule="(self.platform == 'Azure') == has(self.azure)",message="spec.azure must be set when spec.platform is Azure, and must be absent otherwise"
// +kubebuilder:validation:XValidation:rule="(self.platform == 'GCP') == has(self.gcp)",message="spec.gcp must be set when spec.platform is GCP, and must be absent otherwise"
// +kubebuilder:validation:XValidation:rule="self.platform != 'Manual' || (has(self.bgp.peerGroups) && size(self.bgp.peerGroups) > 0)",message="spec.bgp.peerGroups is required when spec.platform is Manual"
// +kubebuilder:validation:XValidation:rule="self.platform == 'Manual' || !has(self.bgp.peerGroups) || size(self.bgp.peerGroups) == 0",message="spec.bgp.peerGroups may only be set when spec.platform is Manual"
type CUDNBgpConfigSpec struct {
	// Platform selects the cloud provider integration mode.
	// AWS auto-discovers BGP endpoints from VPC Route Servers.
	// Manual requires explicit peer groups in spec.bgp.peerGroups.
	Platform PlatformType `json:"platform"`
	// BGP holds the BGP speaker configuration, including local ASN,
	// liveness detection, and (under Manual platform) peer groups.
	BGP BGPConfig `json:"bgp"`
	// RouterNodeSelector is a set of labels that identify which cluster nodes
	// act as BGP routers. Must match labels applied to BGP router machine pools.
	RouterNodeSelector map[string]string `json:"routerNodeSelector"`
	// AWS holds the AWS-specific configuration for auto-discovery of BGP
	// infrastructure. Required when platform is AWS; must not be set otherwise.
	// +optional
	AWS *AWSConfig `json:"aws,omitempty"`
	// +optional
	Azure *AzureConfig `json:"azure,omitempty"`
	GCP   *GCPConfig   `json:"gcp,omitempty"`
}

// CUDNBgpConfigStatus defines the observed state of CUDNBgpConfig.
type CUDNBgpConfigStatus struct {
	// Phase is the current lifecycle phase of the BGP configuration.
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
	// PeerGroups is the discovered peering plan: what the operator found in
	// the cloud and rendered into FRRConfigurations. Empty under
	// platform: Manual, where the plan is declared in spec.bgp.peerGroups
	// rather than discovered.
	// +optional
	// +listType=atomic
	// +kubebuilder:validation:MaxItems=16
	PeerGroups []PeerGroupStatus `json:"peerGroups,omitempty"`
	// FRRProviderOwned is true when this controller added FRR to Network/cluster
	// additionalRoutingCapabilities.providers.
	//
	// Both ownership flags default to false, including on configs that predate
	// this field. If a config was reconciled by an older controller that patched
	// Network/cluster without recording ownership, the field will already be
	// present when this controller re-reads it, so ownership is not re-claimed
	// and the patch is not reverted on deletion. This is deliberate: a present
	// field is indistinguishable from one an administrator set, and reverting it
	// could break unrelated routing.
	// +optional
	FRRProviderOwned bool `json:"frrProviderOwned,omitempty"`
	// RouteAdsOwned is true when this controller set routeAdvertisements to
	// Enabled on Network/cluster. See FRRProviderOwned for the migration and
	// non-ownership semantics that apply equally here.
	// +optional
	RouteAdsOwned bool `json:"routeAdsOwned,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=bgpcc,categories=networking
// +kubebuilder:printcolumn:name="Platform",type="string",JSONPath=".spec.platform"
// +kubebuilder:printcolumn:name="LocalASN",type="integer",JSONPath=".spec.bgp.localASN"
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

// CUDNBgpConfigList contains a list of CUDNBgpConfig.
type CUDNBgpConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CUDNBgpConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CUDNBgpConfig{}, &CUDNBgpConfigList{})
}
