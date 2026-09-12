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

// Package-level e2e helpers shared by the generic (test/e2e) and AWS
// (test/e2e/aws) suites. They live in a regular source file, not a _test.go,
// because Go does not let another package import symbols from test files.
// They return errors rather than depending on a test framework; callers wrap
// them in Expect(...).To(Succeed()).
package e2e

import (
	"context"
	"fmt"
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

// NetworkOperatorGVK is operator.openshift.io/v1 Network for Network/cluster.
var NetworkOperatorGVK = schema.GroupVersionKind{
	Group: "operator.openshift.io", Version: "v1", Kind: "Network",
}

const (
	networkFRRProviderName      = "FRR"
	networkRouteAdsEnabledValue = "Enabled"
)

// CheckConfigNetworkOwnershipExternal returns an error unless BGPCloudConfiguration
// recorded External ownership for both fields, i.e. FRR and route
// advertisements were already enabled before the operator first reconciled.
func CheckConfigNetworkOwnershipExternal(ctx context.Context, c client.Client, configName string) error {
	cfg := &networkingapi.BGPCloudConfiguration{}
	if err := c.Get(ctx, types.NamespacedName{Name: configName}, cfg); err != nil {
		return fmt.Errorf("getting BGPCloudConfiguration %q: %w", configName, err)
	}
	if cfg.Status.FRRProviderOwnership != networkingapi.NetworkPatchOwnershipExternal {
		return fmt.Errorf("FRRProviderOwnership = %q, want External (FRR was pre-enabled; operator must not claim Owned)",
			cfg.Status.FRRProviderOwnership)
	}
	if cfg.Status.RouteAdvertisementsOwnership != networkingapi.NetworkPatchOwnershipExternal {
		return fmt.Errorf("RouteAdvertisementsOwnership = %q, want External "+
			"(routeAdvertisements was pre-enabled; operator must not claim Owned)",
			cfg.Status.RouteAdvertisementsOwnership)
	}
	return nil
}

// CheckExternalNetworkStatePreserved returns an error unless Network/cluster
// still lists FRR and has routeAdvertisements Enabled. After
// BGPCloudConfiguration delete the operator must not unpatch externally
// pre-enabled Network state.
func CheckExternalNetworkStatePreserved(ctx context.Context, c client.Client) error {
	network := &unstructured.Unstructured{}
	network.SetGroupVersionKind(NetworkOperatorGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, network); err != nil {
		return fmt.Errorf("getting Network/cluster: %w", err)
	}

	providers, _, err := unstructured.NestedStringSlice(network.Object,
		"spec", "additionalRoutingCapabilities", "providers")
	if err != nil {
		return fmt.Errorf("reading Network/cluster additionalRoutingCapabilities.providers: %w", err)
	}
	if !slices.Contains(providers, networkFRRProviderName) {
		return fmt.Errorf("network/cluster providers %v missing FRR after BGPCloudConfiguration delete", providers)
	}

	ra, _, err := unstructured.NestedString(network.Object,
		"spec", "defaultNetwork", "ovnKubernetesConfig", "routeAdvertisements")
	if err != nil {
		return fmt.Errorf("reading Network/cluster defaultNetwork.ovnKubernetesConfig.routeAdvertisements: %w", err)
	}
	if ra != networkRouteAdsEnabledValue {
		return fmt.Errorf("routeAdvertisements = %q, want Enabled after BGPCloudConfiguration delete", ra)
	}
	return nil
}
