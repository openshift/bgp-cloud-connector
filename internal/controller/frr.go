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
	"fmt"
	"k8s.io/apimachinery/pkg/api/equality"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

func IsFRRReady(ctx context.Context, c client.Client) (bool, error) {
	ns := &corev1.Namespace{}
	if err := c.Get(ctx, types.NamespacedName{Name: FRRNamespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	pods := &corev1.PodList{}
	if err := c.List(ctx, pods,
		client.InNamespace(FRRNamespace),
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(labels.Set{"app": "frr-k8s"})},
	); err != nil {
		return false, err
	}

	if len(pods.Items) == 0 {
		return false, nil
	}

	for i := range pods.Items {
		if pods.Items[i].Status.Phase != corev1.PodRunning {
			return false, nil
		}
	}
	return true, nil
}

func EnsureFRRConfigurations(ctx context.Context, c client.Client, config *networkingv1alpha1.CUDNBgpConfig) error {
	expected := make(map[string]bool, len(config.Spec.BGP.AvailabilityZones))
	for i, az := range config.Spec.BGP.AvailabilityZones {
		name := fmt.Sprintf("%s%d", FRRConfigNamePrefix, i+1)
		expected[name] = true
		if err := ensureSingleFRRConfiguration(ctx, c, config, az, name); err != nil {
			return fmt.Errorf("AZ %d: %w", i+1, err)
		}
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.List(ctx, list,
		client.InNamespace(FRRNamespace),
		client.MatchingLabels{LabelManagedBy: LabelManagedByVal},
	); err != nil {
		return err
	}
	for i := range list.Items {
		if !expected[list.Items[i].GetName()] {
			if err := c.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("pruning stale %s: %w", list.Items[i].GetName(), err)
			}
		}
	}
	return nil
}

func EnsureFRRConfigurationsFromDiscovery(
	ctx context.Context,
	c client.Client,
	config *networkingv1alpha1.CUDNBgpConfig,
	dr *platform.DiscoveryResult,
) (int, error) {
	azNames := make([]string, 0, len(dr.NeighborsByAZ))
	for az := range dr.NeighborsByAZ {
		azNames = append(azNames, az)
	}
	sort.Strings(azNames)

	expected := make(map[string]bool, len(azNames))
	for i, az := range azNames {
		name := fmt.Sprintf("%s%d", FRRConfigNamePrefix, i+1)
		expected[name] = true

		neighbors := dr.NeighborsByAZ[az]
		syntheticAZ := networkingv1alpha1.AvailabilityZone{
			NodeSelector: map[string]string{"topology.kubernetes.io/zone": az},
		}
		for _, n := range neighbors {
			syntheticAZ.Neighbors = append(syntheticAZ.Neighbors, networkingv1alpha1.BGPNeighbor{
				Address:   n.Address,
				RemoteASN: n.ASN,
			})
		}

		if err := ensureSingleFRRConfiguration(ctx, c, config, syntheticAZ, name); err != nil {
			return 0, fmt.Errorf("AZ %s: %w", az, err)
		}
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.List(ctx, list,
		client.InNamespace(FRRNamespace),
		client.MatchingLabels{LabelManagedBy: LabelManagedByVal},
	); err != nil {
		return 0, err
	}
	for i := range list.Items {
		if !expected[list.Items[i].GetName()] {
			if err := c.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return 0, fmt.Errorf("pruning stale %s: %w", list.Items[i].GetName(), err)
			}
		}
	}
	return len(azNames), nil
}

func ensureSingleFRRConfiguration(
	ctx context.Context,
	c client.Client,
	config *networkingv1alpha1.CUDNBgpConfig,
	az networkingv1alpha1.AvailabilityZone,
	name string,
) error {
	nodeSelector := mergeLabels(config.Spec.RouterNodeSelector, az.NodeSelector)

	neighbors := make([]interface{}, 0, len(az.Neighbors))
	for _, n := range az.Neighbors {
		neighbor := map[string]interface{}{
			"address":   n.Address,
			"asn":       n.RemoteASN,
			"disableMP": true,
			"toReceive": map[string]interface{}{
				"allowed": map[string]interface{}{
					"mode": "all",
				},
			},
		}
		if config.Spec.BGP.LivenessDetection == networkingv1alpha1.LivenessDetectionBFD {
			neighbor["bfdProfile"] = "default"
		}
		neighbors = append(neighbors, neighbor)
	}

	router := map[string]interface{}{
		"asn":       config.Spec.BGP.LocalASN,
		"neighbors": neighbors,
	}

	bgpSpec := map[string]interface{}{
		"routers": []interface{}{router},
	}

	if config.Spec.BGP.LivenessDetection == networkingv1alpha1.LivenessDetectionBFD {
		bgpSpec["bfdProfiles"] = []interface{}{
			map[string]interface{}{
				"name":             "default",
				"receiveInterval":  int64(300),
				"transmitInterval": int64(300),
				"detectMultiplier": int64(3),
			},
		}
	}

	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "frrk8s.metallb.io/v1beta1",
			"kind":       "FRRConfiguration",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": FRRNamespace,
				"labels": map[string]interface{}{
					LabelManagedBy: LabelManagedByVal,
				},
			},
			"spec": map[string]interface{}{
				"nodeSelector": map[string]interface{}{
					"matchLabels": toInterfaceMap(nodeSelector),
				},
				"bgp": bgpSpec,
			},
		},
	}

	return createOrUpdate(ctx, c, obj)
}

func DeleteFRRConfigurations(ctx context.Context, c client.Client) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.List(ctx, list,
		client.InNamespace(FRRNamespace),
		client.MatchingLabels{LabelManagedBy: LabelManagedByVal},
	); err != nil {
		return err
	}
	for i := range list.Items {
		if err := c.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func mergeLabels(base, overlay map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(overlay))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range overlay {
		merged[k] = v
	}
	return merged
}

func toInterfaceMap(m map[string]string) map[string]interface{} {
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		result[k] = v
	}
	return result
}

func createOrUpdate(ctx context.Context, c client.Client, obj *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(obj.GroupVersionKind())
	key := types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}

	err := c.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		return c.Create(ctx, obj)
	}
	if err != nil {
		return err
	}

	if specEqual(existing, obj) && labelsSatisfied(existing.GetLabels(), obj.GetLabels()) {
		return nil
	}

	obj.SetResourceVersion(existing.GetResourceVersion())
	return c.Update(ctx, obj)
}

// specEqual reports whether the object already has the spec we want.
//
// Without this, createOrUpdate rewrote its object on every reconcile
// whether or not anything had changed. Both controllers watch what they
// write, so each write returned immediately as an event: on a live cluster
// the routing controller reconciled about twice a second, indefinitely,
// rewriting the CUDN every time. Nothing converged because nothing was
// meant to; the write itself was the trigger.
func specEqual(existing, desired *unstructured.Unstructured) bool {
	existingSpec, _, _ := unstructured.NestedMap(existing.Object, "spec")
	desiredSpec, _, _ := unstructured.NestedMap(desired.Object, "spec")
	return equality.Semantic.DeepEqual(existingSpec, desiredSpec)
}

// labelsSatisfied reports whether every label we care about is already set
// as we want it. Labels added by anyone else are ignored, so their presence
// does not send us back into the loop this exists to prevent.
func labelsSatisfied(existing, desired map[string]string) bool {
	for k, v := range desired {
		if existing[k] != v {
			return false
		}
	}
	return true
}
