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

	SingletonName          = "cluster"
	FRRNamespace           = "openshift-frr-k8s"
	FRRConfigNamePrefix    = "cudn-bgp-az-"
	CUDNNamePrefix         = "cluster-udn-"
	RouteAdvertisementName = "cudn-bgp-route-advertisements"

	LabelManagedBy    = "app.kubernetes.io/managed-by"
	LabelManagedByVal = "cudn-bgp-routing-operator"
	LabelCUDN         = "cluster-udn"
	LabelAdvertise    = "advertise"
	LabelPrimaryUDN   = "k8s.ovn.org/primary-user-defined-network"
)

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
