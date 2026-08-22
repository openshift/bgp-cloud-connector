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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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
	if err := c.Get(context.Background(), types.NamespacedName{Name: "prod"}, updated); err != nil {
		t.Fatalf("failed to get routing: %v", err)
	}
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
	if err := c.Get(context.Background(), types.NamespacedName{Name: "prod"}, updated); err != nil {
		t.Fatalf("failed to get routing: %v", err)
	}
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
	// DuplicateNetwork is terminal — no requeue after status update.
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for terminal DuplicateNetwork, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpRouting{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "prod"}, updated); err != nil {
		t.Fatalf("failed to get routing: %v", err)
	}
	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
}

// After the conflicting CR is deleted, the degraded duplicate recovers to Ready
// on the next reconcile (triggered in production by enqueueAllRoutings).
func TestRoutingReconcile_DuplicateNetwork_RecoversOnConflictDelete(t *testing.T) {
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
		WithObjects(existing, duplicate, config, ns).
		WithStatusSubresource(existing, duplicate, config).
		Build()

	r := &CUDNBgpRoutingReconciler{Client: c, Scheme: s}

	// First reconcile: DuplicateNetwork → Degraded, no requeue.
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})
	if err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for terminal DuplicateNetwork, got %v", result.RequeueAfter)
	}
	before := &networkingv1alpha1.CUDNBgpRouting{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "prod"}, before); err != nil {
		t.Fatalf("failed to get routing: %v", err)
	}
	if before.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded after duplicate, got %s", before.Status.Phase)
	}

	// Simulate conflict resolution: delete the conflicting CR.
	// In production this triggers enqueueAllRoutings which re-enqueues "prod".
	if err := c.Delete(context.Background(), existing); err != nil {
		t.Fatalf("failed to delete existing-prod: %v", err)
	}

	// Re-reconcile "prod" (as the secondary watch would trigger).
	result2, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})
	if err != nil {
		t.Fatalf("recovery reconcile error: %v", err)
	}
	if result2.RequeueAfter != 5*time.Minute {
		t.Errorf("expected 5m resync after recovery, got %v", result2.RequeueAfter)
	}

	recovered := &networkingv1alpha1.CUDNBgpRouting{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "prod"}, recovered); err != nil {
		t.Fatalf("failed to get routing: %v", err)
	}
	if recovered.Status.Phase != networkingv1alpha1.PhaseReady {
		t.Errorf("expected Ready after conflict resolution, got %s", recovered.Status.Phase)
	}
}

// NamespaceNotReady stays transient (regression guard — must still be 30s).
func TestRoutingReconcile_NamespaceNotReady_StillTransient(t *testing.T) {
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
		t.Errorf("NamespaceNotReady must remain transient (30s), got %v", result.RequeueAfter)
	}
}

// TestEnqueueAllRoutings verifies the mapper returns a reconcile.Request for every CR.
func TestEnqueueAllRoutings(t *testing.T) {
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

	reqs := r.enqueueAllRoutings(context.Background(), routing1)
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(reqs))
	}
	names := map[string]bool{}
	for _, req := range reqs {
		names[req.Name] = true
	}
	if !names["prod"] || !names["staging"] {
		t.Errorf("expected requests for prod and staging, got %v", names)
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

// TestRoutingReconcile_CUDNSpecInvalid_NoRequeue: an invalid CUDN spec (e.g. bad CIDR)
// is terminal — the user must correct spec.network in the CUDNBgpRouting.
func TestRoutingReconcile_CUDNSpecInvalid_NoRequeue(t *testing.T) {
	routing := newTestCUDNBgpRouting()
	routing.Finalizers = []string{RoutingFinalizerName}
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

	invalidErr := apierrors.NewInvalid(
		schema.GroupKind{Group: "k8s.ovn.org", Kind: "ClusterUserDefinedNetwork"},
		"cluster-udn-prod",
		nil,
	)

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routing, config, ns).
		WithStatusSubresource(routing, config).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetName() == CUDNNamePrefix+"prod" {
					return invalidErr
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()

	r := &CUDNBgpRoutingReconciler{Client: c, Scheme: s}
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for terminal CUDNSpecInvalid, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpRouting{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "prod"}, updated); err != nil {
		t.Fatalf("failed to get routing: %v", err)
	}
	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCUDNCreated {
			if cond.Reason != ReasonCUDNSpecInvalid {
				t.Errorf("expected reason CUDNSpecInvalid, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("CUDNCreated condition with CUDNSpecInvalid reason not found")
}
