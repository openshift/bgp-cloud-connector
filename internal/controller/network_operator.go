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
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// getNetworkCluster fetches Network/cluster. Returns (nil, nil) when not found.
func getNetworkCluster(ctx context.Context, c client.Client) (*unstructured.Unstructured, error) {
	network := &unstructured.Unstructured{}
	network.SetGroupVersionKind(NetworkGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: SingletonName}, network); err != nil {
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

// ReadNetworkOperatorState returns Network/cluster along with whether FRR is in
// additionalRoutingCapabilities.providers and whether routeAdvertisements is Enabled.
// Use the flags to decide which fields this controller should claim, and hand the
// returned object to PatchNetworkOperator so the patch reuses this single read.
// Returns (nil, false, false, nil) when Network/cluster does not exist.
func ReadNetworkOperatorState(ctx context.Context, c client.Client) (network *unstructured.Unstructured, frrInProviders bool, routeAdsEnabled bool, err error) {
	network, err = getNetworkCluster(ctx, c)
	if err != nil || network == nil {
		return nil, false, false, err
	}
	providers, err := readProviders(network)
	if err != nil {
		return nil, false, false, err
	}
	ra, err := readRouteAds(network)
	if err != nil {
		return nil, false, false, err
	}
	return network, slices.Contains(providers, FRRProviderName), ra == RouteAdvertisementsOn, nil
}

// PatchNetworkOperator applies only the requested changes to Network/cluster, using
// network as returned by ReadNetworkOperatorState:
// patchFRR merges FRR into additionalRoutingCapabilities.providers (preserving others);
// patchRouteAds sets routeAdvertisements to Enabled. No-op when both flags are false.
func PatchNetworkOperator(ctx context.Context, c client.Client, network *unstructured.Unstructured, patchFRR, patchRouteAds bool) error {
	if !patchFRR && !patchRouteAds {
		return nil
	}

	spec := map[string]interface{}{}

	if patchFRR {
		var providers []string
		if network != nil {
			existing, err := readProviders(network)
			if err != nil {
				return err
			}
			providers = existing
		}
		if !slices.Contains(providers, FRRProviderName) {
			providers = append(providers, FRRProviderName)
		}
		spec["additionalRoutingCapabilities"] = map[string]interface{}{
			"providers": providers,
		}
	}

	if patchRouteAds {
		spec["defaultNetwork"] = map[string]interface{}{
			"ovnKubernetesConfig": map[string]interface{}{
				"routeAdvertisements": RouteAdvertisementsOn,
			},
		}
	}

	patchBytes, err := json.Marshal(map[string]interface{}{"spec": spec})
	if err != nil {
		return err
	}
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(NetworkGVK)
	target.SetName(SingletonName)
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
		filtered := slices.DeleteFunc(existing, func(p string) bool { return p == FRRProviderName })
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
	target.SetName(SingletonName)
	return c.Patch(ctx, target, client.RawPatch(types.MergePatchType, patchBytes))
}
