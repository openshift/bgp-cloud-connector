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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		if !isFRRConfigurationOwnedBy(&list.Items[i], config) {
			continue
		}
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

	setFRRConfigurationOwnerReference(obj, config)
	return createOrUpdate(ctx, c, obj)
}

func DeleteFRRConfigurations(ctx context.Context, c client.Client, config *networkingv1alpha1.CUDNBgpConfig) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.List(ctx, list, client.InNamespace(FRRNamespace)); err != nil {
		return err
	}
	for i := range list.Items {
		if !isFRRConfigurationOwnedBy(&list.Items[i], config) {
			continue
		}
		if err := c.Delete(ctx, &list.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// anyFRRConfigurationsExist reports whether any FRRConfiguration not owned by
// this CUDNBgpConfig still exists. Objects owned by this config (including those
// terminating after DeleteFRRConfigurations) are ignored so they cannot block
// reverting the Network/cluster patch.
func anyFRRConfigurationsExist(ctx context.Context, c client.Client, config *networkingv1alpha1.CUDNBgpConfig) (bool, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.List(ctx, list); err != nil {
		return false, err
	}
	for i := range list.Items {
		if isFRRConfigurationOwnedBy(&list.Items[i], config) {
			continue
		}
		return true, nil
	}
	return false, nil
}

func setFRRConfigurationOwnerReference(obj *unstructured.Unstructured, config *networkingv1alpha1.CUDNBgpConfig) {
	if config.UID == "" {
		return
	}
	controller := true
	ref := metav1.OwnerReference{
		APIVersion: networkingv1alpha1.GroupVersion.String(),
		Kind:       "CUDNBgpConfig",
		Name:       config.Name,
		UID:        config.UID,
		Controller: &controller,
	}
	refs := obj.GetOwnerReferences()
	for i := range refs {
		if refs[i].Kind == "CUDNBgpConfig" && refs[i].Name == config.Name {
			refs[i] = ref
			obj.SetOwnerReferences(refs)
			return
		}
	}
	obj.SetOwnerReferences(append(refs, ref))
}

func isFRRConfigurationOwnedBy(obj *unstructured.Unstructured, config *networkingv1alpha1.CUDNBgpConfig) bool {
	if config.UID == "" {
		return false
	}
	for _, ref := range obj.GetOwnerReferences() {
		if ref.APIVersion == networkingv1alpha1.GroupVersion.String() &&
			ref.Kind == "CUDNBgpConfig" &&
			ref.Name == config.Name &&
			ref.UID == config.UID {
			return true
		}
	}
	return false
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

	obj.SetResourceVersion(existing.GetResourceVersion())
	// Both controllers watch what they write here, so skip the write when nothing we manage changed to avoid re-triggering reconcile.
	if specEqual(existing, obj) && labelsSatisfied(existing.GetLabels(), obj.GetLabels()) && ownerRefsSatisfied(existing, obj) {
		return nil
	}
	// The write replaces metadata wholesale, so carry forward anything we don't manage ourselves (e.g. a foreign label, or ovn-kubernetes' own finalizer/annotations on a CUDN).
	obj.SetLabels(mergeLabels(existing.GetLabels(), obj.GetLabels()))
	obj.SetAnnotations(mergeLabels(existing.GetAnnotations(), obj.GetAnnotations()))
	return c.Update(ctx, obj)
}

// specEqual reports whether every field we set in desired already has that
// value in existing. Fields we never set are ignored: the API server adds
// its own (e.g. FRRConfiguration defaults neighbors[].dualStackAddressFamily),
// and comparing those exactly would never match, rewriting the object forever.
func specEqual(existing, desired *unstructured.Unstructured) bool {
	existingSpec, _, _ := unstructured.NestedMap(existing.Object, "spec")
	desiredSpec, _, _ := unstructured.NestedMap(desired.Object, "spec")
	return specSatisfied(existingSpec, desiredSpec)
}

func specSatisfied(existing, desired interface{}) bool {
	switch want := desired.(type) {
	case map[string]interface{}:
		have, ok := existing.(map[string]interface{})
		if !ok {
			return false
		}
		for k, v := range want {
			if !specSatisfied(have[k], v) {
				return false
			}
		}
		return true
	case []interface{}:
		have, ok := existing.([]interface{})
		if !ok || len(have) != len(want) {
			return false
		}
		for i := range want {
			if !specSatisfied(have[i], want[i]) {
				return false
			}
		}
		return true
	default:
		return apiequality.Semantic.DeepEqual(existing, desired)
	}
}

// labelsSatisfied checks only that desired labels are present; extra labels already on existing (e.g. from another controller) are left alone.
func labelsSatisfied(existing, desired map[string]string) bool {
	for k, v := range desired {
		if existing[k] != v {
			return false
		}
	}
	return true
}

func ownerRefsSatisfied(existing, desired *unstructured.Unstructured) bool {
	want := desired.GetOwnerReferences()
	if len(want) == 0 {
		return true
	}
	have := existing.GetOwnerReferences()
	for _, ref := range want {
		found := false
		for _, h := range have {
			if h.APIVersion == ref.APIVersion && h.Kind == ref.Kind && h.Name == ref.Name && h.UID == ref.UID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
