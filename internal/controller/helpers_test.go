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

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = networkingv1alpha1.AddToScheme(s)
	return s
}

func TestValidateNamespaceLabels_Found(t *testing.T) {
	existing := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app1",
			Labels: map[string]string{
				LabelPrimaryUDN: "",
				LabelCUDN:       "prod",
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

	config := &networkingv1alpha1.CUDNBgpConfig{
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			Platform: networkingv1alpha1.PlatformManual,
			BGP: networkingv1alpha1.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingv1alpha1.LivenessDetectionBFD,
				PeerGroups: []networkingv1alpha1.PeerGroup{
					{
						NodeSelector: map[string]string{"bgp_router_subnet": "1"},
						Neighbors: []networkingv1alpha1.BGPNeighbor{
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
	if err := c.Get(ctx, types.NamespacedName{Name: "cudn-bgp-1", Namespace: FRRNamespace}, obj); err != nil {
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
				"name":      "cudn-bgp-3",
				"namespace": FRRNamespace,
				"labels":    map[string]interface{}{LabelManagedBy: LabelManagedByVal},
			},
			"spec": map[string]interface{}{},
		},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(ns, stale).Build()
	ctx := context.Background()

	config := &networkingv1alpha1.CUDNBgpConfig{
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			Platform: networkingv1alpha1.PlatformManual,
			BGP: networkingv1alpha1.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingv1alpha1.LivenessDetectionBGPKeepalive,
				PeerGroups: []networkingv1alpha1.PeerGroup{
					{
						NodeSelector: map[string]string{"bgp_router_subnet": "1"},
						Neighbors:    []networkingv1alpha1.BGPNeighbor{{Address: "10.0.1.47", RemoteASN: 64512}},
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
	if err := c.Get(ctx, types.NamespacedName{Name: "cudn-bgp-1", Namespace: FRRNamespace}, obj); err != nil {
		t.Error("cudn-bgp-1 should exist")
	}

	staleObj := &unstructured.Unstructured{}
	staleObj.SetGroupVersionKind(FRRConfigurationGVK)
	err := c.Get(ctx, types.NamespacedName{Name: "cudn-bgp-3", Namespace: FRRNamespace}, staleObj)
	if err == nil {
		t.Error("cudn-bgp-3 should have been pruned")
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

	config := &networkingv1alpha1.CUDNBgpConfig{
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			Platform: networkingv1alpha1.PlatformManual,
			BGP: networkingv1alpha1.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingv1alpha1.LivenessDetectionBGPKeepalive,
				PeerGroups: []networkingv1alpha1.PeerGroup{
					{
						NodeSelector: map[string]string{"bgp_router_subnet": "1"},
						Neighbors:    []networkingv1alpha1.BGPNeighbor{{Address: "10.0.1.47", RemoteASN: 64512}},
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

	routerNodes := map[string]string{"networking.openshift.io/cudn-bgp-router": ""}
	config := &networkingv1alpha1.CUDNBgpConfig{
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			Platform:           networkingv1alpha1.PlatformAWS,
			BGP:                networkingv1alpha1.BGPConfig{LocalASN: 65001},
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
	if obj.GetName() != "cudn-bgp-1" {
		t.Errorf("name = %q, want cudn-bgp-1", obj.GetName())
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

// --- Network/cluster builders used across config controller tests ---

func newNetworkObject(spec map[string]interface{}) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       spec,
		},
	}
}

func newEmptyNetwork() *unstructured.Unstructured {
	return newNetworkObject(map[string]interface{}{})
}

func newFRREnabledNetwork() *unstructured.Unstructured {
	return newNetworkObject(map[string]interface{}{
		"additionalRoutingCapabilities": map[string]interface{}{
			"providers": []interface{}{FRRProviderName},
		},
		"defaultNetwork": map[string]interface{}{
			"ovnKubernetesConfig": map[string]interface{}{
				"routeAdvertisements": RouteAdvertisementsOn,
			},
		},
	})
}

func mustNetworkProviders(t *testing.T, network *unstructured.Unstructured) []string {
	providers, found, err := unstructured.NestedStringSlice(network.Object, "spec", "additionalRoutingCapabilities", "providers")
	if err != nil {
		t.Fatalf("reading additionalRoutingCapabilities.providers: %v", err)
	}
	if !found {
		t.Fatal("spec.additionalRoutingCapabilities.providers not found")
	}
	return providers
}

func mustNetworkRouteAds(t *testing.T, network *unstructured.Unstructured) string {
	ra, found, err := unstructured.NestedString(network.Object, "spec", "defaultNetwork", "ovnKubernetesConfig", "routeAdvertisements")
	if err != nil {
		t.Fatalf("reading routeAdvertisements: %v", err)
	}
	if !found {
		t.Fatal("spec.defaultNetwork.ovnKubernetesConfig.routeAdvertisements not found")
	}
	return ra
}

// --- ReadNetworkOwnership ---

func TestReadNetworkOwnership_NetworkNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).Build()
	frrPresent, routeAdsOn, err := ReadNetworkOwnership(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if frrPresent || routeAdsOn {
		t.Error("expected (false, false) when Network/cluster does not exist")
	}
}

func TestReadNetworkOwnership_NoProviders(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).
		WithObjects(newNetworkObject(map[string]interface{}{})).Build()
	frrPresent, routeAdsOn, err := ReadNetworkOwnership(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if frrPresent || routeAdsOn {
		t.Error("expected (false, false) when providers list is absent")
	}
}

func TestReadNetworkOwnership_FRRProviderPresentRouteAdsDisabled(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).
		WithObjects(newNetworkObject(map[string]interface{}{
			"additionalRoutingCapabilities": map[string]interface{}{
				"providers": []interface{}{FRRProviderName},
			},
		})).Build()
	frrPresent, routeAdsOn, err := ReadNetworkOwnership(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !frrPresent {
		t.Error("expected frrPresent=true when FRR is in providers")
	}
	if routeAdsOn {
		t.Error("expected routeAdsOn=false when routeAdvertisements is not Enabled")
	}
}

func TestReadNetworkOwnership_FullyEnabled(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).
		WithObjects(newFRREnabledNetwork()).Build()
	frrPresent, routeAdsOn, err := ReadNetworkOwnership(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !frrPresent {
		t.Error("expected frrPresent=true")
	}
	if !routeAdsOn {
		t.Error("expected routeAdsOn=true")
	}
}

// --- UnpatchNetworkOperator ---

func TestUnpatchNetworkOperator_NetworkNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).Build()
	if err := UnpatchNetworkOperator(context.Background(), c, true, true); err != nil {
		t.Fatalf("expected nil when Network/cluster not found, got: %v", err)
	}
}

func TestUnpatchNetworkOperator_RemovesFRRAndClearsRouteAds(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).
		WithObjects(newFRREnabledNetwork()).Build()
	if err := UnpatchNetworkOperator(context.Background(), c, true, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	network := &unstructured.Unstructured{}
	network.SetGroupVersionKind(NetworkGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, network); err != nil {
		t.Fatalf("failed to read Network: %v", err)
	}
	for _, p := range mustNetworkProviders(t, network) {
		if p == FRRProviderName {
			t.Error("FRR should be removed from providers after unpatch")
		}
	}
	if got := mustNetworkRouteAds(t, network); got != "" {
		t.Errorf("routeAdvertisements = %q, want empty after unpatch", got)
	}
}

func TestUnpatchNetworkOperator_PreservesOtherProviders(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).
		WithObjects(newNetworkObject(map[string]interface{}{
			"additionalRoutingCapabilities": map[string]interface{}{
				"providers": []interface{}{"OtherProvider", FRRProviderName},
			},
			"defaultNetwork": map[string]interface{}{
				"ovnKubernetesConfig": map[string]interface{}{"routeAdvertisements": RouteAdvertisementsOn},
			},
		})).Build()
	if err := UnpatchNetworkOperator(context.Background(), c, true, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	network := &unstructured.Unstructured{}
	network.SetGroupVersionKind(NetworkGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, network); err != nil {
		t.Fatalf("failed to read Network: %v", err)
	}
	providers := mustNetworkProviders(t, network)
	hasFRR, hasOther := false, false
	for _, p := range providers {
		if p == FRRProviderName {
			hasFRR = true
		}
		if p == "OtherProvider" {
			hasOther = true
		}
	}
	if hasFRR {
		t.Error("FRR should be removed from providers")
	}
	if !hasOther {
		t.Error("OtherProvider should be preserved after unpatch")
	}
	if got := mustNetworkRouteAds(t, network); got != "" {
		t.Errorf("routeAdvertisements = %q, want empty after unpatch", got)
	}
}

func TestUnpatchNetworkOperator_Idempotent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).
		WithObjects(newFRREnabledNetwork()).Build()
	ctx := context.Background()
	if err := UnpatchNetworkOperator(ctx, c, true, true); err != nil {
		t.Fatalf("first unpatch: %v", err)
	}
	if err := UnpatchNetworkOperator(ctx, c, true, true); err != nil {
		t.Fatalf("second unpatch: %v", err)
	}
	network := &unstructured.Unstructured{}
	network.SetGroupVersionKind(NetworkGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, network); err != nil {
		t.Fatalf("failed to read Network: %v", err)
	}
	for _, p := range mustNetworkProviders(t, network) {
		if p == FRRProviderName {
			t.Error("FRR should be absent after the second unpatch")
		}
	}
	if got := mustNetworkRouteAds(t, network); got != "" {
		t.Errorf("routeAdvertisements = %q, want empty after the second unpatch", got)
	}
}

func TestUnpatchNetworkOperator_OnlyClearsRouteAdsWhenNotOwningProvider(t *testing.T) {
	// FRR was pre-existing in providers; we only own routeAdvertisements.
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).
		WithObjects(newFRREnabledNetwork()).Build()
	if err := UnpatchNetworkOperator(context.Background(), c, false, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	network := &unstructured.Unstructured{}
	network.SetGroupVersionKind(NetworkGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, network); err != nil {
		t.Fatalf("failed to read Network: %v", err)
	}
	providers := mustNetworkProviders(t, network)
	hasFRR := false
	for _, p := range providers {
		if p == FRRProviderName {
			hasFRR = true
		}
	}
	if !hasFRR {
		t.Error("FRR should remain in providers when FRRProviderOwned=false")
	}
	if got := mustNetworkRouteAds(t, network); got != "" {
		t.Errorf("routeAdvertisements = %q, want empty when RouteAdsOwned=true", got)
	}
}

func TestUnpatchNetworkOperator_OnlyRemovesFRRWhenNotOwningRouteAds(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).
		WithObjects(newFRREnabledNetwork()).Build()
	if err := UnpatchNetworkOperator(context.Background(), c, true, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	network := &unstructured.Unstructured{}
	network.SetGroupVersionKind(NetworkGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, network); err != nil {
		t.Fatalf("failed to read Network: %v", err)
	}
	for _, p := range mustNetworkProviders(t, network) {
		if p == FRRProviderName {
			t.Error("FRR should be removed when FRRProviderOwned=true")
		}
	}
	if mustNetworkRouteAds(t, network) != RouteAdvertisementsOn {
		t.Error("routeAdvertisements should remain Enabled when RouteAdsOwned=false")
	}
}

// --- anyFRRConfigurationsExist ---

func TestAnyFRRConfigurationsExist_Empty(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).Build()
	got, err := anyFRRConfigurationsExist(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("expected false when no FRRConfiguration objects exist")
	}
}

func TestAnyFRRConfigurationsExist_ManagedExists(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1", "kind": "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name": "cudn-bgp-1", "namespace": FRRNamespace,
			"labels": map[string]interface{}{LabelManagedBy: LabelManagedByVal},
		},
	}}
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).WithObjects(obj).Build()
	got, err := anyFRRConfigurationsExist(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got {
		t.Error("expected false when only a managed FRRConfiguration exists")
	}
}

func TestAnyFRRConfigurationsExist_UnmanagedExists(t *testing.T) {
	managed := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1", "kind": "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name": "cudn-bgp-1", "namespace": FRRNamespace,
			"labels": map[string]interface{}{LabelManagedBy: LabelManagedByVal},
		},
	}}
	external := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1", "kind": "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name": "user-frr", "namespace": FRRNamespace,
			"labels": map[string]interface{}{"owner": "user"},
		},
	}}
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).WithObjects(managed, external).Build()
	got, err := anyFRRConfigurationsExist(context.Background(), c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Error("expected true when an unmanaged FRRConfiguration exists alongside a managed one")
	}
}
