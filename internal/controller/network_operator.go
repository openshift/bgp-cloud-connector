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
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// errNoNetworkCluster reports that the cluster singleton this operator patches
// does not exist, which no retry here can fix.
var errNoNetworkCluster = fmt.Errorf("network %q not found", SingletonName)

// getNetworkCluster fetches Network/cluster. Returns (nil, nil) when not found.
func getNetworkCluster(ctx context.Context, c client.Client) (*unstructured.Unstructured, error) {
	network := &unstructured.Unstructured{}
	network.SetGroupVersionKind(NetworkGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: SingletonName}, network); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting Network/cluster: %w", err)
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

type networkPatchResult struct {
	enabledFRRProvider bool
	enabledRouteAds    bool
}

// PatchNetworkOperator applies only the requested changes to Network/cluster.
// The returned flags report what this reconcile actually enabled after any
// conflict retries.
func PatchNetworkOperator(ctx context.Context, c client.Client, network *unstructured.Unstructured, patchFRR, patchRouteAds bool) (networkPatchResult, error) {
	if !patchFRR && !patchRouteAds {
		return networkPatchResult{}, nil
	}

	current := network
	if current == nil {
		return networkPatchResult{}, errNoNetworkCluster
	}

	result := networkPatchResult{}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		result = networkPatchResult{}
		spec := map[string]interface{}{}

		if patchFRR {
			existing, err := readProviders(current)
			if err != nil {
				return err
			}
			if !slices.Contains(existing, FRRProviderName) {
				spec["additionalRoutingCapabilities"] = map[string]interface{}{
					"providers": append(existing, FRRProviderName),
				}
				result.enabledFRRProvider = true
			}
		}

		if patchRouteAds {
			routeAds, err := readRouteAds(current)
			if err != nil {
				return err
			}
			if routeAds != RouteAdvertisementsOn {
				spec["defaultNetwork"] = map[string]interface{}{
					"ovnKubernetesConfig": map[string]interface{}{
						"routeAdvertisements": RouteAdvertisementsOn,
					},
				}
				result.enabledRouteAds = true
			}
		}

		if len(spec) == 0 {
			return nil
		}

		patchBytes, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{"resourceVersion": current.GetResourceVersion()},
			"spec":     spec,
		})
		if err != nil {
			return fmt.Errorf("building Network/cluster patch: %w", err)
		}
		target := &unstructured.Unstructured{}
		target.SetGroupVersionKind(NetworkGVK)
		target.SetName(SingletonName)
		err = c.Patch(ctx, target, client.RawPatch(types.MergePatchType, patchBytes))
		if apierrors.IsConflict(err) {
			latest, getErr := getNetworkCluster(ctx, c)
			if getErr != nil {
				return getErr
			}
			if latest == nil {
				return errNoNetworkCluster
			}
			current = latest
		}
		return err
	})
	return result, err
}

// UnpatchNetworkOperator reverts only the fields this controller owned:
//   - removeFRRProvider: remove FRR from additionalRoutingCapabilities.providers
//   - disableRouteAds: set routeAdvertisements back to Disabled
//
// Each field is re-read and skipped when it is already off, so a repeated or
// partially applied revert writes nothing. routeAdvertisements is set to
// Disabled rather than removed: Disabled is the API's own off value, and it is
// what an absent field means to the network operator.
func UnpatchNetworkOperator(ctx context.Context, c client.Client, removeFRRProvider, disableRouteAds bool) error {
	if !removeFRRProvider && !disableRouteAds {
		return nil
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
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
			if slices.Contains(existing, FRRProviderName) {
				filtered := slices.DeleteFunc(existing, func(p string) bool { return p == FRRProviderName })
				if len(filtered) == 0 {
					// Drop the whole stanza rather than leave an empty providers list.
					spec["additionalRoutingCapabilities"] = nil
				} else {
					spec["additionalRoutingCapabilities"] = map[string]interface{}{
						"providers": filtered,
					}
				}
			}
		}

		if disableRouteAds {
			routeAds, err := readRouteAds(network)
			if err != nil {
				return err
			}
			if routeAds == RouteAdvertisementsOn {
				spec["defaultNetwork"] = map[string]interface{}{
					"ovnKubernetesConfig": map[string]interface{}{
						"routeAdvertisements": RouteAdvertisementsDisabled,
					},
				}
			}
		}

		if len(spec) == 0 {
			return nil
		}

		patchBytes, err := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{"resourceVersion": network.GetResourceVersion()},
			"spec":     spec,
		})
		if err != nil {
			return fmt.Errorf("building Network/cluster unpatch: %w", err)
		}

		target := &unstructured.Unstructured{}
		target.SetGroupVersionKind(NetworkGVK)
		target.SetName(SingletonName)
		return c.Patch(ctx, target, client.RawPatch(types.MergePatchType, patchBytes))
	})
}
