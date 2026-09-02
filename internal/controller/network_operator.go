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

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// getNetworkCluster fetches Network/cluster. Returns (nil, nil) when not found.
func getNetworkCluster(ctx context.Context, c client.Client) (*unstructured.Unstructured, error) {
	network := &unstructured.Unstructured{}
	network.SetGroupVersionKind(NetworkGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, network); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return network, nil
}

// readProviders returns the current providers slice from Network/cluster spec.
func readProviders(network *unstructured.Unstructured) ([]string, error) {
	providers, _, err := unstructured.NestedStringSlice(network.Object,
		"spec", "additionalRoutingCapabilities", "providers")
	if err != nil {
		return nil, fmt.Errorf("reading additionalRoutingCapabilities.providers: %w", err)
	}
	return providers, nil
}

// readRouteAds returns the current routeAdvertisements value from Network/cluster spec.
func readRouteAds(network *unstructured.Unstructured) (string, error) {
	ra, _, err := unstructured.NestedString(network.Object,
		"spec", "defaultNetwork", "ovnKubernetesConfig", "routeAdvertisements")
	if err != nil {
		return "", fmt.Errorf("reading routeAdvertisements: %w", err)
	}
	return ra, nil
}

// ReadNetworkOwnership returns the pre-existing state of the two fields this
// controller manages. Use the return values to decide which fields the controller
// needs to claim ownership of before calling PatchNetworkOperator.
// Returns (false, false, nil) when Network/cluster does not exist.
func ReadNetworkOwnership(ctx context.Context, c client.Client) (frrInProviders bool, routeAdsEnabled bool, err error) {
	network, err := getNetworkCluster(ctx, c)
	if err != nil || network == nil {
		return false, false, err
	}
	providers, err := readProviders(network)
	if err != nil {
		return false, false, err
	}
	for _, p := range providers {
		if p == FRRProviderName {
			frrInProviders = true
			break
		}
	}
	ra, err := readRouteAds(network)
	if err != nil {
		return false, false, err
	}
	routeAdsEnabled = ra == RouteAdvertisementsOn
	return frrInProviders, routeAdsEnabled, nil
}

// PatchNetworkOperator merges FRR into additionalRoutingCapabilities.providers
// (preserving any pre-existing providers) and sets routeAdvertisements to Enabled.
func PatchNetworkOperator(ctx context.Context, c client.Client) error {
	network, err := getNetworkCluster(ctx, c)
	if err != nil {
		return err
	}

	var mergedProviders []interface{}
	if network != nil {
		existing, err := readProviders(network)
		if err != nil {
			return err
		}
		for _, p := range existing {
			mergedProviders = append(mergedProviders, p)
		}
	}
	hasFRR := false
	for _, p := range mergedProviders {
		if p == FRRProviderName {
			hasFRR = true
			break
		}
	}
	if !hasFRR {
		mergedProviders = append(mergedProviders, FRRProviderName)
	}

	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"additionalRoutingCapabilities": map[string]interface{}{
				"providers": mergedProviders,
			},
			"defaultNetwork": map[string]interface{}{
				"ovnKubernetesConfig": map[string]interface{}{
					"routeAdvertisements": RouteAdvertisementsOn,
				},
			},
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(NetworkGVK)
	target.SetName("cluster")
	return c.Patch(ctx, target, client.RawPatch(types.MergePatchType, patchBytes))
}

// UnpatchNetworkOperator reverts only the fields this controller owned:
//   - removeFRRProvider: remove FRR from additionalRoutingCapabilities.providers
//   - clearRouteAds: clear routeAdvertisements
func UnpatchNetworkOperator(ctx context.Context, c client.Client, removeFRRProvider, clearRouteAds bool) error {
	if !removeFRRProvider && !clearRouteAds {
		return nil
	}
	network, err := getNetworkCluster(ctx, c)
	if err != nil || network == nil {
		return err
	}

	spec := map[string]interface{}{}

	if removeFRRProvider {
		existing, err := readProviders(network)
		if err != nil {
			return err
		}
		filtered := make([]interface{}, 0, len(existing))
		for _, p := range existing {
			if p != FRRProviderName {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) == 0 {
			spec["additionalRoutingCapabilities"] = nil
		} else {
			spec["additionalRoutingCapabilities"] = map[string]interface{}{
				"providers": filtered,
			}
		}
	}

	if clearRouteAds {
		spec["defaultNetwork"] = map[string]interface{}{
			"ovnKubernetesConfig": map[string]interface{}{
				"routeAdvertisements": RouteAdvertisementsDisabled,
			},
		}
	}

	patchBytes, err := json.Marshal(map[string]interface{}{"spec": spec})
	if err != nil {
		return err
	}

	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(NetworkGVK)
	target.SetName("cluster")
	return c.Patch(ctx, target, client.RawPatch(types.MergePatchType, patchBytes))
}
