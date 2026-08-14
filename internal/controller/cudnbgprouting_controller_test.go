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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

func routingTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = networkingv1alpha1.AddToScheme(s)

	s.AddKnownTypeWithName(CUDNNetworkGVK.GroupVersion().WithKind("ClusterUserDefinedNetwork"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(CUDNNetworkGVK.GroupVersion().WithKind("ClusterUserDefinedNetworkList"), &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(RouteAdvertisementsGVK.GroupVersion().WithKind("RouteAdvertisements"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(RouteAdvertisementsGVK.GroupVersion().WithKind("RouteAdvertisementsList"), &unstructured.UnstructuredList{})
	return s
}

func newTestCUDNBgpRouting() *networkingv1alpha1.CUDNBgpRouting {
	return &networkingv1alpha1.CUDNBgpRouting{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prod",
		},
		Spec: networkingv1alpha1.CUDNBgpRoutingSpec{
			Network: networkingv1alpha1.NetworkConfig{
				Name:    "prod",
				Subnets: []string{"10.100.0.0/16"},
			},
		},
	}
}

func newReadyCUDNBgpConfig() *networkingv1alpha1.CUDNBgpConfig {
	return &networkingv1alpha1.CUDNBgpConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			BGP: networkingv1alpha1.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingv1alpha1.LivenessDetectionBGPKeepalive,
				AvailabilityZones: []networkingv1alpha1.AvailabilityZone{
					{
						NodeSelector: map[string]string{"bgp_router_subnet": "1"},
						Neighbors:    []networkingv1alpha1.BGPNeighbor{{Address: "10.0.1.47", RemoteASN: 64512}},
					},
				},
			},
			RouterNodeSelector: map[string]string{"bgp_router": "true"},
		},
		Status: networkingv1alpha1.CUDNBgpConfigStatus{
			Phase: networkingv1alpha1.PhaseReady,
		},
	}
}

func TestRoutingReconcile_FullReconcile(t *testing.T) {
	routing := newTestCUDNBgpRouting()
	config := newReadyCUDNBgpConfig()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app1",
			Labels: map[string]string{
				LabelPrimaryUDN: "",
				LabelCUDN:       "prod",
			},
		},
	}

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routing, config, ns).
		WithStatusSubresource(routing, config).
		Build()

	r := &CUDNBgpRoutingReconciler{Client: c, Scheme: s}

	// First reconcile adds finalizer
	_, _ = r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})

	// Second reconcile does full 2-phase
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if result.RequeueAfter != 5*time.Minute {
		t.Errorf("expected 5m resync requeue, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpRouting{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "prod"}, updated)
	if updated.Status.Phase != networkingv1alpha1.PhaseReady {
		t.Errorf("expected Ready, got %s", updated.Status.Phase)
	}
	if len(updated.Status.Conditions) != 2 {
		t.Errorf("expected 2 conditions, got %d", len(updated.Status.Conditions))
	}

	// Verify CUDN created
	cudn := &unstructured.Unstructured{}
	cudn.SetGroupVersionKind(CUDNNetworkGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster-udn-prod"}, cudn); err != nil {
		t.Fatalf("CUDN not created: %v", err)
	}

	// Verify RouteAdvertisements created
	ra := &unstructured.Unstructured{}
	ra.SetGroupVersionKind(RouteAdvertisementsGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: RouteAdvertisementName}, ra); err != nil {
		t.Fatalf("RouteAdvertisements not created: %v", err)
	}
}

func TestRoutingReconcile_NoNamespace(t *testing.T) {
	routing := newTestCUDNBgpRouting()
	routing.Finalizers = []string{RoutingFinalizerName}
	config := newReadyCUDNBgpConfig()

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routing, config).
		WithStatusSubresource(routing, config).
		Build()

	r := &CUDNBgpRoutingReconciler{Client: c, Scheme: s}
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected 30s degraded requeue, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpRouting{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "prod"}, updated)
	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
}

func TestRoutingReconcile_DeleteLastRemovesRA(t *testing.T) {
	now := metav1.Now()
	routing := newTestCUDNBgpRouting()
	routing.Finalizers = []string{RoutingFinalizerName}
	routing.DeletionTimestamp = &now

	ra := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "k8s.ovn.org/v1",
			"kind":       "RouteAdvertisements",
			"metadata": map[string]interface{}{
				"name":   RouteAdvertisementName,
				"labels": map[string]interface{}{LabelManagedBy: LabelManagedByVal},
			},
		},
	}

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routing, ra).
		WithStatusSubresource(routing).
		Build()

	r := &CUDNBgpRoutingReconciler{Client: c, Scheme: s}
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// RA should be deleted since this was the last routing CR
	raCheck := &unstructured.Unstructured{}
	raCheck.SetGroupVersionKind(RouteAdvertisementsGVK)
	err = c.Get(context.Background(), types.NamespacedName{Name: RouteAdvertisementName}, raCheck)
	if err == nil {
		t.Error("RouteAdvertisements should be deleted when last routing CR is removed")
	}
}

func TestRoutingReconcile_DeleteKeepsRAWhenOthersExist(t *testing.T) {
	now := metav1.Now()
	routing := newTestCUDNBgpRouting()
	routing.Finalizers = []string{RoutingFinalizerName}
	routing.DeletionTimestamp = &now

	other := &networkingv1alpha1.CUDNBgpRouting{
		ObjectMeta: metav1.ObjectMeta{Name: "staging"},
		Spec: networkingv1alpha1.CUDNBgpRoutingSpec{
			Network: networkingv1alpha1.NetworkConfig{
				Name: "staging", Subnets: []string{"10.200.0.0/16"},
			},
		},
	}

	ra := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "k8s.ovn.org/v1",
			"kind":       "RouteAdvertisements",
			"metadata": map[string]interface{}{
				"name":   RouteAdvertisementName,
				"labels": map[string]interface{}{LabelManagedBy: LabelManagedByVal},
			},
		},
	}

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routing, other, ra).
		WithStatusSubresource(routing, other).
		Build()

	r := &CUDNBgpRoutingReconciler{Client: c, Scheme: s}
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// RA should still exist since "staging" routing CR remains
	raCheck := &unstructured.Unstructured{}
	raCheck.SetGroupVersionKind(RouteAdvertisementsGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: RouteAdvertisementName}, raCheck); err != nil {
		t.Error("RouteAdvertisements should be kept when other routing CRs exist")
	}
}

func TestRoutingReconcile_DuplicateNetworkName(t *testing.T) {
	existing := &networkingv1alpha1.CUDNBgpRouting{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-prod"},
		Spec: networkingv1alpha1.CUDNBgpRoutingSpec{
			Network: networkingv1alpha1.NetworkConfig{
				Name: "prod", Subnets: []string{"10.100.0.0/16"},
			},
		},
	}
	duplicate := newTestCUDNBgpRouting()
	duplicate.Finalizers = []string{RoutingFinalizerName}
	config := newReadyCUDNBgpConfig()

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(existing, duplicate, config).
		WithStatusSubresource(existing, duplicate, config).
		Build()

	r := &CUDNBgpRoutingReconciler{Client: c, Scheme: s}
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected 30s degraded requeue, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpRouting{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "prod"}, updated)
	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
}

func TestMapCUDNToRouting_ManagedCUDN(t *testing.T) {
	routing := newTestCUDNBgpRouting()
	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(routing).Build()
	r := &CUDNBgpRoutingReconciler{Client: c, Scheme: s}

	cudn := &unstructured.Unstructured{}
	cudn.SetName(CUDNNamePrefix + "prod")
	cudn.SetLabels(map[string]string{LabelManagedBy: LabelManagedByVal})

	requests := r.mapCUDNToRouting(context.Background(), cudn)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "prod" {
		t.Errorf("expected request for 'prod', got %q", requests[0].Name)
	}
}

func TestMapCUDNToRouting_UnmanagedCUDN(t *testing.T) {
	routing := newTestCUDNBgpRouting()
	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(routing).Build()
	r := &CUDNBgpRoutingReconciler{Client: c, Scheme: s}

	cudn := &unstructured.Unstructured{}
	cudn.SetName(CUDNNamePrefix + "prod")
	cudn.SetLabels(map[string]string{"other": "label"})

	requests := r.mapCUDNToRouting(context.Background(), cudn)
	if len(requests) != 0 {
		t.Errorf("expected 0 requests for unmanaged CUDN, got %d", len(requests))
	}
}

func TestMapRAToRouting_ManagedRA(t *testing.T) {
	routing1 := newTestCUDNBgpRouting()
	routing2 := &networkingv1alpha1.CUDNBgpRouting{
		ObjectMeta: metav1.ObjectMeta{Name: "staging"},
		Spec: networkingv1alpha1.CUDNBgpRoutingSpec{
			Network: networkingv1alpha1.NetworkConfig{Name: "staging", Subnets: []string{"10.200.0.0/16"}},
		},
	}
	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(routing1, routing2).Build()
	r := &CUDNBgpRoutingReconciler{Client: c, Scheme: s}

	ra := &unstructured.Unstructured{}
	ra.SetName(RouteAdvertisementName)
	ra.SetLabels(map[string]string{LabelManagedBy: LabelManagedByVal})

	requests := r.mapRAToRouting(context.Background(), ra)
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}
}

func TestMapRAToRouting_UnmanagedRA(t *testing.T) {
	routing := newTestCUDNBgpRouting()
	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(routing).Build()
	r := &CUDNBgpRoutingReconciler{Client: c, Scheme: s}

	ra := &unstructured.Unstructured{}
	ra.SetName("some-other-ra")
	ra.SetLabels(map[string]string{})

	requests := r.mapRAToRouting(context.Background(), ra)
	if len(requests) != 0 {
		t.Errorf("expected 0 requests for unmanaged RA, got %d", len(requests))
	}
}

func TestRoutingReconcile_ConfiguredResyncInterval(t *testing.T) {
	routing := newTestCUDNBgpRouting()
	config := newReadyCUDNBgpConfig()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app1",
			Labels: map[string]string{
				LabelPrimaryUDN: "",
				LabelCUDN:       "prod",
			},
		},
	}

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routing, config, ns).
		WithStatusSubresource(routing, config).
		Build()

	r := &CUDNBgpRoutingReconciler{Client: c, Scheme: s, ResyncInterval: 30 * time.Second}

	_, _ = r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected configured 30s resync requeue, got %v", result.RequeueAfter)
	}
}

// createOrUpdate rewrote its object on every call, whether or not anything
// had changed. Both controllers watch what they write, so each write came
// straight back as an event: on a live cluster the routing controller
// reconciled roughly twice a second, indefinitely, rewriting the CUDN each
// time. Writing only on a real difference breaks the loop.
func TestCreateOrUpdate_NoWriteWhenUnchanged(t *testing.T) {
	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).Build()

	desired := func() *unstructured.Unstructured {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(CUDNNetworkGVK)
		o.SetName("cluster-udn-prod")
		o.SetLabels(map[string]string{LabelManagedBy: LabelManagedByVal})
		_ = unstructured.SetNestedMap(o.Object, map[string]interface{}{
			"topology": "Layer2",
		}, "spec")
		return o
	}

	if err := createOrUpdate(context.Background(), c, desired()); err != nil {
		t.Fatalf("create: %v", err)
	}

	read := &unstructured.Unstructured{}
	read.SetGroupVersionKind(CUDNNetworkGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster-udn-prod"}, read); err != nil {
		t.Fatalf("get after create: %v", err)
	}
	first := read.GetResourceVersion()

	// Same desired state, twice more.
	for i := 0; i < 2; i++ {
		if err := createOrUpdate(context.Background(), c, desired()); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}

	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster-udn-prod"}, read); err != nil {
		t.Fatalf("get after updates: %v", err)
	}
	if read.GetResourceVersion() != first {
		t.Errorf("object was rewritten despite being unchanged: resourceVersion %s -> %s",
			first, read.GetResourceVersion())
	}
}

// ...but a genuine change must still be written.
func TestCreateOrUpdate_WritesWhenChanged(t *testing.T) {
	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).Build()

	build := func(topology string) *unstructured.Unstructured {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(CUDNNetworkGVK)
		o.SetName("cluster-udn-prod")
		_ = unstructured.SetNestedMap(o.Object, map[string]interface{}{
			"topology": topology,
		}, "spec")
		return o
	}

	if err := createOrUpdate(context.Background(), c, build("Layer2")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := createOrUpdate(context.Background(), c, build("Layer3")); err != nil {
		t.Fatalf("update: %v", err)
	}

	read := &unstructured.Unstructured{}
	read.SetGroupVersionKind(CUDNNetworkGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster-udn-prod"}, read); err != nil {
		t.Fatalf("get: %v", err)
	}
	topology, _, _ := unstructured.NestedString(read.Object, "spec", "topology")
	if topology != "Layer3" {
		t.Errorf("expected the change to be written, got topology %q", topology)
	}
}
