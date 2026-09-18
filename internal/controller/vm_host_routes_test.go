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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

func vmHostRouteTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = networkingapi.AddToScheme(s)
	s.AddKnownTypeWithName(FRRConfigurationGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(VirtualMachineInstanceGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(VirtualMachineInstanceGVK.GroupVersion().WithKind("VirtualMachineInstanceList"), &unstructured.UnstructuredList{})
	return s
}

func testVMI(namespace, name, node string, addresses ...string) *unstructured.Unstructured {
	values := make([]interface{}, len(addresses))
	for i := range addresses {
		values[i] = addresses[i]
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachineInstance",
		"metadata":   map[string]interface{}{"namespace": namespace, "name": name},
		"status": map[string]interface{}{
			"phase":    "Running",
			"nodeName": node,
			"interfaces": []interface{}{map[string]interface{}{
				"ipAddresses": values,
			}},
		},
	}}
}

func testRouterNode(name, zone string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
		corev1.LabelHostname: name, "bgp_router": "true", "zone": zone,
	}}}
}

func testVMNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "vms", Labels: map[string]string{
		LabelClusterUDN: "prod", LabelPrimaryUDN: "",
	}}}
}

func TestEnsureVMHostRoutesAggregatesDualStackRoutesPerNode(t *testing.T) {
	ctx := context.Background()
	routing := newTestBGPRouting()
	routing.Spec.Network.Subnets = []string{"10.100.0.0/16", "fd00:100::/64"}
	config := newReadyBGPCloudConfiguration()
	node := testRouterNode("worker-a", "a")
	config.Spec.BGP.PeerGroups[0].NodeSelector = map[string]string{"zone": "a"}
	config.Spec.BGP.PeerGroups[0].Neighbors = append(config.Spec.BGP.PeerGroups[0].Neighbors,
		networkingapi.BGPNeighbor{Address: "fd00:1::47", RemoteASN: 64512})
	objects := []client.Object{
		testVMNamespace(), node,
		testVMI("vms", "vm-one", node.Name, "10.100.0.4", "fd00:100::4", "169.254.0.1"),
		testVMI("vms", "vm-two", node.Name, "10.100.0.5", "fd00:100::5"),
		testVMI("other", "ignored", node.Name, "10.100.0.6"),
	}
	c := fake.NewClientBuilder().WithScheme(vmHostRouteTestScheme()).WithObjects(objects...).Build()

	count, pending, err := EnsureVMHostRoutes(ctx, c, routing, config)
	if err != nil {
		t.Fatalf("EnsureVMHostRoutes: %v", err)
	}
	if count != 4 {
		t.Fatalf("route count = %d, want 4", count)
	}
	if pending {
		t.Fatal("routes unexpectedly pending")
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(FRRConfigurationGVK)
	key := types.NamespacedName{Name: vmHostRouteConfigurationName(routing.Name, node.Name), Namespace: FRRNamespace}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatalf("get FRRConfiguration: %v", err)
	}
	routers, _, _ := unstructured.NestedSlice(got.Object, "spec", "bgp", "routers")
	router := routers[0].(map[string]interface{})
	want := []interface{}{"10.100.0.4/32", "10.100.0.5/32", "fd00:100::4/128", "fd00:100::5/128"}
	if actual := router["prefixes"]; !reflect.DeepEqual(actual, want) {
		t.Fatalf("prefixes = %#v, want %#v", actual, want)
	}
	neighbors := router["neighbors"].([]interface{})
	if len(neighbors) != 2 {
		t.Fatalf("neighbors = %d, want 2", len(neighbors))
	}
	v4Allowed := neighbors[0].(map[string]interface{})["toAdvertise"].(map[string]interface{})["allowed"].(map[string]interface{})
	if v4Allowed["mode"] != "filtered" || !reflect.DeepEqual(v4Allowed["prefixes"], []interface{}{"10.100.0.4/32", "10.100.0.5/32"}) {
		t.Fatalf("unexpected IPv4 export filter: %#v", v4Allowed)
	}
	v6Allowed := neighbors[1].(map[string]interface{})["toAdvertise"].(map[string]interface{})["allowed"].(map[string]interface{})
	if v6Allowed["mode"] != "filtered" || !reflect.DeepEqual(v6Allowed["prefixes"], []interface{}{"fd00:100::4/128", "fd00:100::5/128"}) {
		t.Fatalf("unexpected IPv6 export filter: %#v", v6Allowed)
	}
	hostname, _, _ := unstructured.NestedString(got.Object, "spec", "nodeSelector", "matchLabels", corev1.LabelHostname)
	if hostname != node.Name {
		t.Fatalf("hostname selector = %q, want %q", hostname, node.Name)
	}
}

func TestEnsureVMHostRoutesRejectsPrefixWithoutSameFamilyNeighbor(t *testing.T) {
	ctx := context.Background()
	routing := newTestBGPRouting()
	routing.Spec.Network.Subnets = []string{"fd00:100::/64"}
	config := newReadyBGPCloudConfiguration()
	config.Spec.BGP.PeerGroups[0].NodeSelector = nil
	node := testRouterNode("worker-a", "a")
	c := fake.NewClientBuilder().WithScheme(vmHostRouteTestScheme()).WithObjects(
		testVMNamespace(), node, testVMI("vms", "vm", node.Name, "fd00:100::4"),
	).Build()

	_, _, err := EnsureVMHostRoutes(ctx, c, routing, config)
	if err == nil || !strings.Contains(err.Error(), "no BGP neighbor of the same address family") {
		t.Fatalf("expected address-family error, got %v", err)
	}
}

func TestEnsureVMHostRoutesRetriesWhileVMIAddressIsPending(t *testing.T) {
	ctx := context.Background()
	routing := newTestBGPRouting()
	config := newReadyBGPCloudConfiguration()
	config.Spec.BGP.PeerGroups[0].NodeSelector = nil
	node := testRouterNode("worker-a", "a")
	c := fake.NewClientBuilder().WithScheme(vmHostRouteTestScheme()).WithObjects(
		testVMNamespace(), node, testVMI("vms", "vm", node.Name),
	).Build()

	count, pending, err := EnsureVMHostRoutes(ctx, c, routing, config)
	if err != nil {
		t.Fatalf("EnsureVMHostRoutes: %v", err)
	}
	if count != 0 || !pending {
		t.Fatalf("count = %d, pending = %t; want 0, true", count, pending)
	}
}

func TestEnsureVMHostRoutesDoesNotRetryTerminalVMI(t *testing.T) {
	ctx := context.Background()
	routing := newTestBGPRouting()
	vmi := testVMI("vms", "vm", "worker-a")
	vmi.Object["status"].(map[string]interface{})["phase"] = "Failed"
	c := fake.NewClientBuilder().WithScheme(vmHostRouteTestScheme()).WithObjects(
		testVMNamespace(), vmi,
	).Build()

	count, pending, err := EnsureVMHostRoutes(ctx, c, routing, newReadyBGPCloudConfiguration())
	if err != nil {
		t.Fatalf("EnsureVMHostRoutes: %v", err)
	}
	if count != 0 || pending {
		t.Fatalf("count = %d, pending = %t; want 0, false", count, pending)
	}
}

func TestEnsureVMHostRoutesMovesRouteWithVMI(t *testing.T) {
	ctx := context.Background()
	routing := newTestBGPRouting()
	config := newReadyBGPCloudConfiguration()
	config.Spec.BGP.PeerGroups[0].NodeSelector = nil
	nodeA := testRouterNode("worker-a", "a")
	nodeB := testRouterNode("worker-b", "b")
	vmi := testVMI("vms", "vm", nodeA.Name, "10.100.0.4")
	c := fake.NewClientBuilder().WithScheme(vmHostRouteTestScheme()).WithObjects(testVMNamespace(), nodeA, nodeB, vmi).Build()

	if _, _, err := EnsureVMHostRoutes(ctx, c, routing, config); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	oldKey := types.NamespacedName{Name: vmHostRouteConfigurationName(routing.Name, nodeA.Name), Namespace: FRRNamespace}

	vmi.Object["status"].(map[string]interface{})["nodeName"] = nodeB.Name
	if err := c.Update(ctx, vmi); err != nil {
		t.Fatalf("move VMI: %v", err)
	}
	if _, _, err := EnsureVMHostRoutes(ctx, c, routing, config); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	old := &unstructured.Unstructured{}
	old.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, oldKey, old); !apierrors.IsNotFound(err) {
		t.Fatalf("old node configuration was not pruned: %v", err)
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(FRRConfigurationGVK)
	newKey := types.NamespacedName{Name: vmHostRouteConfigurationName(routing.Name, nodeB.Name), Namespace: FRRNamespace}
	if err := c.Get(ctx, newKey, current); err != nil {
		t.Fatalf("new node configuration missing: %v", err)
	}
}

func TestEnsureVMHostRoutesDoesNotPruneAnotherRouting(t *testing.T) {
	ctx := context.Background()
	routing := newTestBGPRouting()
	other := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1", "kind": "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name": "other-routing", "namespace": FRRNamespace,
			"labels":      map[string]interface{}{LabelManagedBy: LabelManagedByVMHostRoutes},
			"annotations": map[string]interface{}{AnnotationBGPRouting: "staging"},
		},
	}}
	c := fake.NewClientBuilder().WithScheme(vmHostRouteTestScheme()).WithObjects(other).Build()
	if _, _, err := EnsureVMHostRoutes(ctx, c, routing, newReadyBGPCloudConfiguration()); err != nil {
		t.Fatalf("EnsureVMHostRoutes: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(other), got); err != nil {
		t.Fatalf("configuration for another BGPRouting was pruned: %v", err)
	}
}

func TestEnsureVMHostRoutesWithdrawsRouteFromNonRouterNode(t *testing.T) {
	ctx := context.Background()
	routing := newTestBGPRouting()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", Labels: map[string]string{corev1.LabelHostname: "worker"}}}
	stale := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1", "kind": "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name": vmHostRouteConfigurationName(routing.Name, node.Name), "namespace": FRRNamespace,
			"labels":      map[string]interface{}{LabelManagedBy: LabelManagedByVMHostRoutes},
			"annotations": map[string]interface{}{AnnotationBGPRouting: routing.Name},
		},
	}}
	c := fake.NewClientBuilder().WithScheme(vmHostRouteTestScheme()).WithObjects(
		testVMNamespace(), node, testVMI("vms", "vm", node.Name, "10.100.0.4"), stale,
	).Build()

	_, _, err := EnsureVMHostRoutes(ctx, c, routing, newReadyBGPCloudConfiguration())
	if err == nil || !strings.Contains(err.Error(), node.Name) {
		t.Fatalf("expected non-router node error, got %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(stale), got); !apierrors.IsNotFound(err) {
		t.Fatalf("stale route was not withdrawn: %v", err)
	}
}

func TestEffectivePeerGroupsUsesDiscoveredCloudState(t *testing.T) {
	config := newReadyBGPCloudConfiguration()
	config.Spec.Platform = networkingapi.PlatformAWS
	config.Spec.BGP.PeerGroups = nil
	config.Status.PeerGroups = []networkingapi.PeerGroupStatus{{
		Key: "zone-a", NodeSelector: map[string]string{"zone": "a"},
		Neighbors: []networkingapi.BGPNeighbor{{Address: "10.0.1.10", RemoteASN: 64512}},
	}}
	groups := effectivePeerGroups(config)
	if len(groups) != 1 || groups[0].Key != "zone-a" || groups[0].Neighbors[0].Address != "10.0.1.10" {
		t.Fatalf("unexpected discovered groups: %#v", groups)
	}
}
