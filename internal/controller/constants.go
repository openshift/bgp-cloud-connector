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

package controller

import "k8s.io/apimachinery/pkg/runtime/schema"

const (
	ConfigFinalizerName  = "networking.openshift.io/cudnbgpconfig"
	RoutingFinalizerName = "networking.openshift.io/cudnbgprouting"

	ConditionDeletionBlocked = "DeletionBlocked"

	SingletonName = "cluster"
	FRRNamespace  = "openshift-frr-k8s"
	// DefaultOperatorNamespace is where the operator runs unless the
	// Deployment says otherwise through POD_NAMESPACE.
	DefaultOperatorNamespace    = "openshift-cudn-bgp-routing"
	FRRConfigNamePrefix         = "cudn-bgp-"
	FRRProviderName             = "FRR"
	CUDNNamePrefix              = "cluster-udn-"
	RouteAdvertisementName      = "cudn-bgp-route-advertisements"
	RouteAdvertisementsOn       = "Enabled"
	RouteAdvertisementsDisabled = "Disabled"

	// RawFRRConfigPriority orders this raw block against the raw blocks of
	// other FRRConfigurations, a higher value being appended later. It says
	// nothing about where the block sits relative to the generated
	// configuration: frr-k8s appends every raw block after the one it renders
	// from the type-safe API, whatever the priority.
	RawFRRConfigPriority = 20

	LabelManagedBy    = "app.kubernetes.io/managed-by"
	LabelManagedByVal = "cudn-bgp-routing-operator"
	LabelCUDN         = "cluster-udn"
	LabelAdvertise    = "advertise"
	LabelPrimaryUDN   = "k8s.ovn.org/primary-user-defined-network"
)

// Condition reason constants
const (
	// Terminal degraded reasons — no requeue, user action required.
	ReasonInvalidName             = "InvalidName"
	ReasonDuplicateNetwork        = "DuplicateNetwork"
	ReasonCloudCredentialsInvalid = "CloudCredentialsInvalid"
	ReasonRouteServerNotFound     = "RouteServerNotFound"
	ReasonCUDNSpecInvalid         = "CUDNSpecInvalid"

	// Transient degraded reasons
	ReasonPatchFailed          = "PatchFailed"
	ReasonNetworkReadFailed    = "NetworkReadFailed"
	ReasonCheckFailed          = "CheckFailed"
	ReasonCloudDiscoveryFailed = "CloudDiscoveryFailed"
	ReasonApplyFailed          = "ApplyFailed"
	ReasonCloudReconcileFailed = "CloudReconcileFailed"
	ReasonNamespaceNotReady    = "NamespaceNotReady"
	ReasonCUDNFailed           = "CUDNFailed"
	ReasonRAFailed             = "RAFailed"

	// Success / informational reasons
	ReasonPatched                 = "Patched"
	ReasonWaitingForFRR           = "WaitingForFRR"
	ReasonFRRReady                = "Ready"
	ReasonDiscovered              = "Discovered"
	ReasonApplied                 = "Applied"
	ReasonReconciled              = "Reconciled"
	ReasonCreated                 = "Created"
	ReasonRoutingCRsExist         = "RoutingCRsExist"
	ReasonExternalFRRConfigsExist = "ExternalFRRConfigsExist"
)

// TerminalDegradedReasons returns condition reasons that must not schedule RequeueAfter.
func TerminalDegradedReasons() map[string]struct{} {
	return map[string]struct{}{
		ReasonInvalidName:             {},
		ReasonDuplicateNetwork:        {},
		ReasonCloudCredentialsInvalid: {},
		ReasonRouteServerNotFound:     {},
		ReasonCUDNSpecInvalid:         {},
	}
}

// IsTerminalDegradedReason reports whether reason must not schedule RequeueAfter.
func IsTerminalDegradedReason(reason string) bool {
	_, ok := TerminalDegradedReasons()[reason]
	return ok
}

var (
	NetworkGVK = schema.GroupVersionKind{
		Group: "operator.openshift.io", Version: "v1", Kind: "Network",
	}
	FRRConfigurationGVK = schema.GroupVersionKind{
		Group: "frrk8s.metallb.io", Version: "v1beta1", Kind: "FRRConfiguration",
	}
	CUDNNetworkGVK = schema.GroupVersionKind{
		Group: "k8s.ovn.org", Version: "v1", Kind: "ClusterUserDefinedNetwork",
	}
	RouteAdvertisementsGVK = schema.GroupVersionKind{
		Group: "k8s.ovn.org", Version: "v1", Kind: "RouteAdvertisements",
	}
)
