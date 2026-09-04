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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = networkingapi.AddToScheme(s)
	return s
}

func TestValidateNamespaceLabels_Found(t *testing.T) {
	existing := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app1",
			Labels: map[string]string{
				LabelPrimaryUDN: "",
				LabelClusterUDN: "prod",
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(existing).Build()

	if err := ValidateNamespaceLabels(context.Background(), c, "prod"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateNamespaceLabels_NotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).Build()

	err := ValidateNamespaceLabels(context.Background(), c, "prod")
	if err == nil {
		t.Fatal("expected error when no namespace has required labels")
	}
}

func TestEnsureFRRConfigurations_BFDProfile(t *testing.T) {
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ns).Build()
	ctx := context.Background()

	config := &networkingapi.BGPCloudConfiguration{
		Spec: networkingapi.BGPCloudConfigurationSpec{
			Platform: networkingapi.PlatformManual,
			BGP: networkingapi.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingapi.LivenessDetectionBFD,
				PeerGroups: []networkingapi.PeerGroup{
					{
						NodeSelector: map[string]string{"bgp_router_subnet": "1"},
						Neighbors: []networkingapi.BGPNeighbor{
							{Address: "10.0.1.47", RemoteASN: 64512},
						},
					},
				},
			},
			RouterNodeSelector: map[string]string{"bgp_router": "true"},
		},
	}

	if err := EnsureFRRConfigurations(ctx, c, config); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-1", Namespace: FRRNamespace}, obj); err != nil {
		t.Fatalf("FRRConfiguration not created: %v", err)
	}

	bfdProfiles, _, _ := unstructured.NestedSlice(obj.Object, "spec", "bgp", "bfdProfiles")
	if len(bfdProfiles) == 0 {
		t.Fatal("expected bfdProfiles when livenessDetection is bfd")
	}

	profile := bfdProfiles[0].(map[string]interface{})
	if profile["name"] != "default" {
		t.Errorf("expected bfd profile name 'default', got %v", profile["name"])
	}
}

func TestEnsureFRRConfigurations_PrunesStale(t *testing.T) {
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}

	stale := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "frrk8s.metallb.io/v1beta1",
			"kind":       "FRRConfiguration",
			"metadata": map[string]interface{}{
				"name":      "bgp-cc-3",
				"namespace": FRRNamespace,
				"labels":    map[string]interface{}{LabelManagedBy: LabelManagedByVal},
			},
			"spec": map[string]interface{}{},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ns, stale).Build()
	ctx := context.Background()

	config := &networkingapi.BGPCloudConfiguration{
		Spec: networkingapi.BGPCloudConfigurationSpec{
			Platform: networkingapi.PlatformManual,
			BGP: networkingapi.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingapi.LivenessDetectionBGPKeepalive,
				PeerGroups: []networkingapi.PeerGroup{
					{
						NodeSelector: map[string]string{"bgp_router_subnet": "1"},
						Neighbors:    []networkingapi.BGPNeighbor{{Address: "10.0.1.47", RemoteASN: 64512}},
					},
				},
			},
			RouterNodeSelector: map[string]string{"bgp_router": "true"},
		},
	}

	if err := EnsureFRRConfigurations(ctx, c, config); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-1", Namespace: FRRNamespace}, obj); err != nil {
		t.Error("bgp-cc-1 should exist")
	}

	staleObj := &unstructured.Unstructured{}
	staleObj.SetGroupVersionKind(FRRConfigurationGVK)
	err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-3", Namespace: FRRNamespace}, staleObj)
	if err == nil {
		t.Error("bgp-cc-3 should have been pruned")
	}
}

func TestEnsureFRRConfigurations_KeepsUnmanagedResources(t *testing.T) {
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}

	userOwned := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "frrk8s.metallb.io/v1beta1",
			"kind":       "FRRConfiguration",
			"metadata": map[string]interface{}{
				"name":      "user-custom-frr",
				"namespace": FRRNamespace,
				"labels":    map[string]interface{}{"owner": "user"},
			},
			"spec": map[string]interface{}{},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ns, userOwned).Build()
	ctx := context.Background()

	config := &networkingapi.BGPCloudConfiguration{
		Spec: networkingapi.BGPCloudConfigurationSpec{
			Platform: networkingapi.PlatformManual,
			BGP: networkingapi.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingapi.LivenessDetectionBGPKeepalive,
				PeerGroups: []networkingapi.PeerGroup{
					{
						NodeSelector: map[string]string{"bgp_router_subnet": "1"},
						Neighbors:    []networkingapi.BGPNeighbor{{Address: "10.0.1.47", RemoteASN: 64512}},
					},
				},
			},
			RouterNodeSelector: map[string]string{"bgp_router": "true"},
		},
	}

	if err := EnsureFRRConfigurations(ctx, c, config); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "user-custom-frr", Namespace: FRRNamespace}, obj); err != nil {
		t.Error("user-owned FRRConfiguration should not be pruned")
	}
}

// TestEnsureFRRConfigurationsFromGroups_SingleRegionalGroup covers the shape
// this whole abstraction exists for: a cloud presenting one pair of addresses
// for the region rather than a pair per zone.
//
// Every other test here renders the AWS shape, one group per zone carrying a
// zone selector, so nothing else shows that a group selecting no nodes of its
// own produces one FRRConfiguration covering every router node. That is what
// GCP and Azure emit, and asserting it here is the difference between the
// claim being tested and being taken on trust.
func TestEnsureFRRConfigurationsFromGroups_SingleRegionalGroup(t *testing.T) {
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ns).Build()
	ctx := context.Background()

	routerNodes := map[string]string{"networking.openshift.io/bgp-cc-router": ""}
	config := &networkingapi.BGPCloudConfiguration{
		Spec: networkingapi.BGPCloudConfigurationSpec{
			Platform:           networkingapi.PlatformAWS,
			BGP:                networkingapi.BGPConfig{LocalASN: 65001},
			RouterNodeSelector: routerNodes,
		},
	}

	// One group, no selector of its own: every router node peers with the
	// same regional pair.
	groups := []platform.PeerGroup{{
		Key: "my-cluster-cudn-cr",
		Neighbors: []platform.DiscoveredNeighbor{
			{Address: "10.0.0.5", ASN: 65000},
			{Address: "10.0.0.6", ASN: 65000},
		},
	}}

	count, err := EnsureFRRConfigurationsFromGroups(ctx, c, config, groups)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("rendered %d configurations, want 1", count)
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.List(ctx, list, client.InNamespace(FRRNamespace)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("got %d FRRConfigurations, want 1", len(list.Items))
	}
	obj := list.Items[0]
	if obj.GetName() != "bgp-cc-1" {
		t.Errorf("name = %q, want bgp-cc-1", obj.GetName())
	}

	// The group narrows nothing, so the selector is exactly the router node
	// selector: every router node, not a zone's worth of them.
	sel, found, err := unstructured.NestedStringMap(obj.Object, "spec", "nodeSelector", "matchLabels")
	if err != nil || !found {
		t.Fatalf("spec.nodeSelector.matchLabels not set (found=%t, err=%v)", found, err)
	}
	if len(sel) != len(routerNodes) {
		t.Fatalf("selector = %v, want %v", sel, routerNodes)
	}
	for k, v := range routerNodes {
		if sel[k] != v {
			t.Errorf("selector[%q] = %q, want %q", k, sel[k], v)
		}
	}

	routers, found, err := unstructured.NestedSlice(obj.Object, "spec", "bgp", "routers")
	if err != nil || !found || len(routers) != 1 {
		t.Fatalf("spec.bgp.routers not a single router (found=%t, err=%v)", found, err)
	}
	neighbors, found, err := unstructured.NestedSlice(routers[0].(map[string]interface{}), "neighbors")
	if err != nil || !found {
		t.Fatalf("neighbors not set (found=%t, err=%v)", found, err)
	}
	if len(neighbors) != 2 {
		t.Fatalf("got %d neighbours, want 2", len(neighbors))
	}
	for i, want := range []string{"10.0.0.5", "10.0.0.6"} {
		if got := neighbors[i].(map[string]interface{})["address"]; got != want {
			t.Errorf("neighbour %d address = %v, want %s", i, got, want)
		}
	}
}

// TestEnsureFRRConfigurationsFromGroups_RawFRRConfig covers a platform whose
// peering needs FRR directives the structured neighbor API cannot express.
//
// GCP is the case: a Cloud Router interface is not on the node's link in the
// way FRR expects, so the session needs disable-connected-check, for which
// frr-k8s has no field. Without somewhere to put it, a platform that needs one
// cannot be implemented behind this renderer at all.
//
// A group that does not ask for one must produce no spec.raw, which is what
// keeps the AWS output unchanged.
func TestEnsureFRRConfigurationsFromGroups_RawFRRConfig(t *testing.T) {
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ns).Build()
	ctx := context.Background()

	config := &networkingapi.BGPCloudConfiguration{
		Spec: networkingapi.BGPCloudConfigurationSpec{
			Platform:           networkingapi.PlatformAWS,
			BGP:                networkingapi.BGPConfig{LocalASN: 65001},
			RouterNodeSelector: map[string]string{"bgp_router": "true"},
		},
	}

	raw := "      router bgp 65001\n       neighbor 10.0.0.5 disable-connected-check\n"
	groups := []platform.PeerGroup{
		{
			Key:          "my-cloud-router",
			Neighbors:    []platform.DiscoveredNeighbor{{Address: "10.0.0.5", ASN: 65000}},
			RawFRRConfig: raw,
		},
		{
			Key:       "no-raw",
			Neighbors: []platform.DiscoveredNeighbor{{Address: "10.0.0.6", ASN: 65000}},
		},
	}

	if _, err := EnsureFRRConfigurationsFromGroups(ctx, c, config, groups); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	withRaw := &unstructured.Unstructured{}
	withRaw.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-1", Namespace: FRRNamespace}, withRaw); err != nil {
		t.Fatalf("bgp-cc-1 not created: %v", err)
	}
	got, found, err := unstructured.NestedString(withRaw.Object, "spec", "raw", "rawConfig")
	if err != nil || !found {
		t.Fatalf("spec.raw.rawConfig not set (found=%t, err=%v)", found, err)
	}
	if got != raw {
		t.Errorf("spec.raw.rawConfig = %q, want %q", got, raw)
	}
	prio, found, err := unstructured.NestedInt64(withRaw.Object, "spec", "raw", "priority")
	if err != nil || !found {
		t.Fatalf("spec.raw.priority not set (found=%t, err=%v)", found, err)
	}
	if prio != RawFRRConfigPriority {
		t.Errorf("spec.raw.priority = %d, want %d", prio, RawFRRConfigPriority)
	}

	withoutRaw := &unstructured.Unstructured{}
	withoutRaw.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-2", Namespace: FRRNamespace}, withoutRaw); err != nil {
		t.Fatalf("bgp-cc-2 not created: %v", err)
	}
	if _, found, _ := unstructured.NestedMap(withoutRaw.Object, "spec", "raw"); found {
		t.Error("spec.raw should be absent when the group asks for no raw config")
	}
}

// TestEnsureFRRConfigurationsFromGroups_EBGPMultiHop covers a neighbour that
// is not on the node's link, which is what an Azure Route Server is: it sits
// in its own subnet, and without multihop the session never establishes.
//
// A neighbour that is on the link must carry no such field at all, which is
// what keeps the AWS output unchanged.
func TestEnsureFRRConfigurationsFromGroups_EBGPMultiHop(t *testing.T) {
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ns).Build()
	ctx := context.Background()

	config := &networkingapi.BGPCloudConfiguration{
		Spec: networkingapi.BGPCloudConfigurationSpec{
			Platform:           networkingapi.PlatformAWS,
			BGP:                networkingapi.BGPConfig{LocalASN: 65001},
			RouterNodeSelector: map[string]string{"bgp_router": "true"},
		},
	}

	groups := []platform.PeerGroup{{
		Key: "route-server",
		Neighbors: []platform.DiscoveredNeighbor{
			{Address: "10.0.1.4", ASN: 65515, EBGPMultiHop: true},
			{Address: "10.0.1.5", ASN: 65515},
		},
	}}

	if _, err := EnsureFRRConfigurationsFromGroups(ctx, c, config, groups); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-1", Namespace: FRRNamespace}, obj); err != nil {
		t.Fatalf("bgp-cc-1 not created: %v", err)
	}
	routers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "bgp", "routers")
	neighbors, _, _ := unstructured.NestedSlice(routers[0].(map[string]interface{}), "neighbors")
	if len(neighbors) != 2 {
		t.Fatalf("got %d neighbours, want 2", len(neighbors))
	}

	first := neighbors[0].(map[string]interface{})
	if got, ok := first["ebgpMultiHop"]; !ok || got != true {
		t.Errorf("off-link neighbour: ebgpMultiHop = %v (present=%t), want true", got, ok)
	}
	second := neighbors[1].(map[string]interface{})
	if _, ok := second["ebgpMultiHop"]; ok {
		t.Error("on-link neighbour should carry no ebgpMultiHop field")
	}
}
