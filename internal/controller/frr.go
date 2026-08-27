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

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
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
	_, err := EnsureFRRConfigurationsFromGroups(ctx, c, config, peerGroupsFromSpec(config))
	return err
}

// peerGroupsFromSpec turns declared peer groups into the same shape a cloud
// discovers, so both paths render through one code path and cannot drift.
//
// The key is the group's position, which is what names the object it becomes.
func peerGroupsFromSpec(config *networkingv1alpha1.CUDNBgpConfig) []platform.PeerGroup {
	groups := make([]platform.PeerGroup, 0, len(config.Spec.BGP.PeerGroups))
	for i, g := range config.Spec.BGP.PeerGroups {
		group := platform.PeerGroup{
			Key:          fmt.Sprintf("%d", i+1),
			NodeSelector: g.NodeSelector,
		}
		for _, n := range g.Neighbors {
			group.Neighbors = append(group.Neighbors, platform.DiscoveredNeighbor{
				Address:      n.Address,
				ASN:          n.RemoteASN,
				EBGPMultiHop: n.EBGPMultiHop,
			})
		}
		groups = append(groups, group)
	}
	return groups
}

// EnsureFRRConfigurationsFromGroups writes one FRRConfiguration per peer group
// and prunes any managed configuration the groups no longer account for.
//
// The generated object name comes from the group's position, so callers must
// pass groups in a stable order.
func EnsureFRRConfigurationsFromGroups(
	ctx context.Context,
	c client.Client,
	config *networkingv1alpha1.CUDNBgpConfig,
	groups []platform.PeerGroup,
) (int, error) {
	expected := make(map[string]bool, len(groups))
	for i, group := range groups {
		name := fmt.Sprintf("%s%d", FRRConfigNamePrefix, i+1)
		expected[name] = true
		if err := ensureSingleFRRConfiguration(ctx, c, config, group, name); err != nil {
			return 0, fmt.Errorf("peer group %s: %w", group.Key, err)
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
	return len(groups), nil
}

func ensureSingleFRRConfiguration(
	ctx context.Context,
	c client.Client,
	config *networkingv1alpha1.CUDNBgpConfig,
	group platform.PeerGroup,
	name string,
) error {
	nodeSelector := mergeLabels(config.Spec.RouterNodeSelector, group.NodeSelector)

	neighbors := make([]interface{}, 0, len(group.Neighbors))
	for _, n := range group.Neighbors {
		neighbor := map[string]interface{}{
			"address":   n.Address,
			"asn":       n.ASN,
			"disableMP": true,
			"toReceive": map[string]interface{}{
				"allowed": map[string]interface{}{
					"mode": "all",
				},
			},
		}
		// Omitted rather than written false: a neighbour on the node's link
		// carries no such field.
		if n.EBGPMultiHop {
			neighbor["ebgpMultiHop"] = true
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

	// Directives frr-k8s has no field for, merged at a lower precedence than
	// the generated configuration above.
	if group.RawFRRConfig != "" {
		if err := unstructured.SetNestedMap(obj.Object, map[string]interface{}{
			"priority":  int64(RawFRRConfigPriority),
			"rawConfig": group.RawFRRConfig,
		}, "spec", "raw"); err != nil {
			return fmt.Errorf("setting spec.raw: %w", err)
		}
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

	if err := c.Get(ctx, key, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return c.Create(ctx, obj)
		}
		return err
	}

	if specUnchanged(existing, obj) {
		return nil
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := c.Get(ctx, key, existing); err != nil {
			return err
		}
		obj.SetResourceVersion(existing.GetResourceVersion())
		return c.Update(ctx, obj)
	})
}

func specUnchanged(existing, desired *unstructured.Unstructured) bool {
	existingSpec := existing.Object["spec"]
	desiredSpec := desired.Object["spec"]
	existingLabels := existing.GetLabels()
	desiredLabels := desired.GetLabels()
	return apiequality.Semantic.DeepEqual(existingSpec, desiredSpec) &&
		apiequality.Semantic.DeepEqual(existingLabels, desiredLabels)
}
