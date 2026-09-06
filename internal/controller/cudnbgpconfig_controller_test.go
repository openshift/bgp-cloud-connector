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
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
	awsplatform "github.com/openshift/bgp-cloud-connector/internal/platform/aws"
)

func configTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = networkingv1alpha1.AddToScheme(s)

	s.AddKnownTypeWithName(NetworkGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(NetworkGVK.GroupVersion().WithKind("NetworkList"), &unstructured.UnstructuredList{})

	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})
	return s
}

func newTestCUDNBgpConfig() *networkingv1alpha1.CUDNBgpConfig {
	return &networkingv1alpha1.CUDNBgpConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			Platform: networkingv1alpha1.PlatformManual,
			BGP: networkingv1alpha1.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingv1alpha1.LivenessDetectionBGPKeepalive,
				PeerGroups: []networkingv1alpha1.PeerGroup{
					{
						NodeSelector: map[string]string{"bgp_router_subnet": "1"},
						Neighbors: []networkingv1alpha1.BGPNeighbor{
							{Address: "10.0.1.47", RemoteASN: 64512},
							{Address: "10.0.1.183", RemoteASN: 64512},
						},
					},
				},
			},
			RouterNodeSelector: map[string]string{"bgp_router": "true"},
		},
	}
}

func TestConfigReconcile_FullReconcile(t *testing.T) {
	config := newTestCUDNBgpConfig()
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}

	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "frr-k8s-pod",
			Namespace: FRRNamespace,
			Labels:    map[string]string{"app": "frr-k8s"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}

	// First reconcile adds finalizer
	_, _ = r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "cluster"},
	})

	// Second reconcile does full 3-phase
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "cluster"},
	})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if result.RequeueAfter != 5*time.Minute {
		t.Errorf("expected 5m resync requeue, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("failed to get config: %v", err)
	}
	if updated.Status.Phase != networkingv1alpha1.PhaseReady {
		t.Errorf("expected phase Ready, got %s", updated.Status.Phase)
	}

	if len(updated.Status.Conditions) != 3 {
		t.Errorf("expected 3 conditions, got %d", len(updated.Status.Conditions))
	}

	// Verify FRRConfiguration was created
	frrConfig := &unstructured.Unstructured{}
	frrConfig.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cudn-bgp-1", Namespace: FRRNamespace}, frrConfig); err != nil {
		t.Fatalf("FRRConfiguration not created: %v", err)
	}
	if updated.Status.FRRProviderOwnership != networkingv1alpha1.NetworkPatchOwnershipOwned ||
		updated.Status.RouteAdvertisementsOwnership != networkingv1alpha1.NetworkPatchOwnershipOwned {
		t.Error("expected both Network fields Owned when neither was pre-existing")
	}
}

// singleton name mismatch → Degraded, no requeue (terminal).
func TestConfigReconcile_InvalidName_NoRequeue(t *testing.T) {
	config := newTestCUDNBgpConfig()
	config.Name = "wrong-name"

	s := configTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "wrong-name"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for terminal InvalidName, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "wrong-name"}, updated); err != nil {
		t.Fatalf("failed to get config: %v", err)
	}
	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected phase Degraded, got %s", updated.Status.Phase)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionNetworkOperatorPatched {
			if cond.Reason != ReasonInvalidName {
				t.Errorf("expected reason InvalidName, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("NetworkOperatorPatched condition with InvalidName reason not found")
}

// second reconcile of InvalidName still returns no requeue (regression guard).
func TestConfigReconcile_InvalidName_SecondReconcileNoRequeue(t *testing.T) {
	config := newTestCUDNBgpConfig()
	config.Name = "wrong-name"

	s := configTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}

	result1, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "wrong-name"},
	})
	if err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}
	if result1.RequeueAfter != 0 {
		t.Errorf("expected no requeue after first reconcile, got %v", result1.RequeueAfter)
	}

	result2, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "wrong-name"},
	})
	if err != nil {
		t.Fatalf("second reconcile error: %v", err)
	}
	if result2.RequeueAfter != 0 {
		t.Errorf("expected no requeue after second reconcile, got %v", result2.RequeueAfter)
	}
}

func TestConfigReconcile_DeleteBlockedByRouting(t *testing.T) {
	now := metav1.Now()
	config := newTestCUDNBgpConfig()
	config.Finalizers = []string{ConfigFinalizerName}
	config.DeletionTimestamp = &now

	routing := &networkingv1alpha1.CUDNBgpRouting{
		ObjectMeta: metav1.ObjectMeta{Name: "prod"},
		Spec: networkingv1alpha1.CUDNBgpRoutingSpec{
			Network: networkingv1alpha1.NetworkConfig{
				Name: "prod", Subnets: []string{"10.100.0.0/16"},
			},
		},
	}

	s := configTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, routing).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "cluster"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when routing CRs still exist")
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("failed to get config: %v", err)
	}
	found := false
	for _, f := range updated.Finalizers {
		if f == ConfigFinalizerName {
			found = true
		}
	}
	if !found {
		t.Error("finalizer should not be removed while routing CRs exist")
	}
}

// --- Phase 4: Controller AWS Integration (Mocked Platform) ---

type mockPlatform struct {
	discoverResult          *platform.DiscoveryResult
	discoverErr             error
	discoverCalled          bool
	discoverCallCount       int
	reconcileNodesCalled    bool
	reconcileNodesCallCount int
	reconcileNodesArgs      []platform.RouterNode
	reconcileNodesErr       error
	cleanupCalled           bool
	cleanupErr              error
}

func (m *mockPlatform) DiscoverEndpoints(_ context.Context) (*platform.DiscoveryResult, error) {
	m.discoverCalled = true
	m.discoverCallCount++
	if m.discoverErr != nil {
		return nil, m.discoverErr
	}
	if m.discoverResult != nil {
		return m.discoverResult, nil
	}
	return &platform.DiscoveryResult{
		PeerGroups: []platform.PeerGroup{
			{
				Key:          "us-east-1a",
				NodeSelector: map[string]string{"topology.kubernetes.io/zone": "us-east-1a"},
				Neighbors:    []platform.DiscoveredNeighbor{{Address: "10.0.1.47", ASN: 64512}},
			},
		},
	}, nil
}

func (m *mockPlatform) ReconcileNodes(_ context.Context, nodes []platform.RouterNode) error {
	m.reconcileNodesCalled = true
	m.reconcileNodesCallCount++
	m.reconcileNodesArgs = nodes
	return m.reconcileNodesErr
}

func (m *mockPlatform) Cleanup(_ context.Context) error {
	m.cleanupCalled = true
	return m.cleanupErr
}

func newTestCUDNBgpConfigWithAWS() *networkingv1alpha1.CUDNBgpConfig {
	return &networkingv1alpha1.CUDNBgpConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
		Spec: networkingv1alpha1.CUDNBgpConfigSpec{
			Platform: networkingv1alpha1.PlatformAWS,
			BGP: networkingv1alpha1.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingv1alpha1.LivenessDetectionBGPKeepalive,
			},
			RouterNodeSelector: map[string]string{"bgp_router": "true"},
			AWS: &networkingv1alpha1.AWSConfig{
				Region:         "us-east-1",
				RouteServerIDs: []string{"rs-1"},
			},
		},
	}
}

func newRouterNode(name, ip, az, providerID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"bgp_router": "true", "topology.kubernetes.io/zone": az},
		},
		Spec: corev1.NodeSpec{ProviderID: providerID},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
		},
	}
}

func TestConfigReconcile_AWSFullReconcile(t *testing.T) {
	mock := &mockPlatform{}
	config := newTestCUDNBgpConfigWithAWS()
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	node := newRouterNode("node-1", "10.0.1.10", "us-east-1a", "aws:///us-east-1a/i-001")

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod, node).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return mock, nil
		},
	}

	// First reconcile adds finalizer
	_, _ = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	// Second reconcile does full 5-phase
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if result.RequeueAfter != 5*time.Minute {
		t.Errorf("expected 5m resync, got %v", result.RequeueAfter)
	}
	if !mock.discoverCalled {
		t.Fatal("expected DiscoverEndpoints to be called")
	}
	if !mock.reconcileNodesCalled {
		t.Fatal("expected ReconcileNodes to be called")
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("failed to get config: %v", err)
	}
	if updated.Status.Phase != networkingv1alpha1.PhaseReady {
		t.Errorf("expected Ready, got %s", updated.Status.Phase)
	}
	if len(updated.Status.Conditions) != 6 {
		t.Errorf("expected 6 conditions, got %d", len(updated.Status.Conditions))
	}
	// The discovered plan is reported cloud-neutrally: what FRR was told to
	// peer with, rather than the route servers one cloud happens to have.
	if len(updated.Status.PeerGroups) != 1 {
		t.Fatalf("expected 1 peer group in status, got %d", len(updated.Status.PeerGroups))
	}
	if got := updated.Status.PeerGroups[0].Key; got != "us-east-1a" {
		t.Errorf("peer group key = %q, want %q", got, "us-east-1a")
	}
	if got := updated.Status.PeerGroups[0].Neighbors; len(got) != 1 || got[0].Address != "10.0.1.47" {
		t.Errorf("peer group neighbours = %v, want one at 10.0.1.47", got)
	}

	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCloudEndpointsDiscovered {
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("expected CloudEndpointsDiscovered=True, got %s", cond.Status)
			}
		}
		if cond.Type == networkingv1alpha1.ConditionCloudResourcesReconciled {
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("expected CloudResourcesReconciled=True, got %s", cond.Status)
			}
		}
		if cond.Type == networkingv1alpha1.ConditionCompleteNodeInventory {
			if cond.Status != metav1.ConditionTrue || cond.Reason != "Complete" {
				t.Errorf("expected CompleteNodeInventory=True/Complete, got %s/%s", cond.Status, cond.Reason)
			}
		}
	}

	// Verify FRR was created from discovery
	frrConfig := &unstructured.Unstructured{}
	frrConfig.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cudn-bgp-1", Namespace: FRRNamespace}, frrConfig); err != nil {
		t.Fatalf("FRRConfiguration not created from discovery: %v", err)
	}
}

func TestConfigReconcile_RepeatedReconcile_DoesNotRewriteFRRConfiguration(t *testing.T) {
	mock := &mockPlatform{}
	config := newTestCUDNBgpConfigWithAWS()
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	node := newRouterNode("node-1", "10.0.1.10", "us-east-1a", "aws:///us-east-1a/i-001")

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod, node).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return mock, nil
		},
	}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}

	// First reconcile adds finalizer; second does the full 5-phase reconcile
	// (matches the two-call convention used by TestConfigReconcile_AWSFullReconcile).
	_, _ = r.Reconcile(context.Background(), req)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	frrConfig := &unstructured.Unstructured{}
	frrConfig.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cudn-bgp-1", Namespace: FRRNamespace}, frrConfig); err != nil {
		t.Fatalf("FRRConfiguration not created from discovery: %v", err)
	}
	resourceVersionAfterFirstPass := frrConfig.GetResourceVersion()
	discoverCallsAfterFirstPass := mock.discoverCallCount
	reconcileNodesCallsAfterFirstPass := mock.reconcileNodesCallCount

	// Reconcile again with no external state changed. This is the scenario
	// that used to loop forever: the FRRConfiguration watch re-enqueues this
	// same request every time createOrUpdate rewrites the object.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile error: %v", err)
	}

	if err := c.Get(context.Background(), types.NamespacedName{Name: "cudn-bgp-1", Namespace: FRRNamespace}, frrConfig); err != nil {
		t.Fatalf("get FRRConfiguration after repeat reconcile: %v", err)
	}
	if frrConfig.GetResourceVersion() != resourceVersionAfterFirstPass {
		t.Fatalf("expected no rewrite of FRRConfiguration on repeated reconcile, resourceVersion changed from %q to %q",
			resourceVersionAfterFirstPass, frrConfig.GetResourceVersion())
	}

	// AWS discovery and node reconciliation are expected to run on every
	// pass (they are not gated by equality) — only the object write should
	// stop. Assert this explicitly so the test's intent can't be misread.
	if mock.discoverCallCount != discoverCallsAfterFirstPass+1 {
		t.Errorf("expected DiscoverEndpoints to still run every reconcile, got %d calls after repeat (was %d)",
			mock.discoverCallCount, discoverCallsAfterFirstPass)
	}
	if mock.reconcileNodesCallCount != reconcileNodesCallsAfterFirstPass+1 {
		t.Errorf("expected ReconcileNodes to still run every reconcile, got %d calls after repeat (was %d)",
			mock.reconcileNodesCallCount, reconcileNodesCallsAfterFirstPass)
	}
}

func TestConfigReconcile_AWSCredentialFailure(t *testing.T) {
	config := newTestCUDNBgpConfigWithAWS()
	config.Finalizers = []string{ConfigFinalizerName}
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return nil, &platform.CredentialError{Msg: "invalid credentials"}
		},
	}

	result, _ := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	// CloudCredentialsInvalid is terminal — user must fix the credentials, not time.
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for terminal CloudCredentialsInvalid, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("failed to get config: %v", err)
	}
	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCloudEndpointsDiscovered {
			if cond.Reason != ReasonCloudCredentialsInvalid {
				t.Errorf("expected reason CloudCredentialsInvalid, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("CloudEndpointsDiscovered condition not found")
}

// TestConfigReconcile_CredentialFailure_ClearsStaleNodeInventory verifies that
// stale CompleteNodeInventory=True and CloudResourcesReconciled=True left by a
// previous successful reconcile do not survive a later credential failure that
// returns before Phase 5 runs.
func TestConfigReconcile_CredentialFailure_ClearsStaleNodeInventory(t *testing.T) {
	config := newTestCUDNBgpConfigWithAWS()
	config.Finalizers = []string{ConfigFinalizerName}
	// Seed stale conditions as if a previous reconcile had reached Phase 5.
	config.Status.Conditions = []metav1.Condition{{
		Type:               networkingv1alpha1.ConditionCompleteNodeInventory,
		Status:             metav1.ConditionTrue,
		Reason:             "Complete",
		Message:            "stale from a previous reconcile",
		LastTransitionTime: metav1.Now(),
	}, {
		Type:               networkingv1alpha1.ConditionCloudResourcesReconciled,
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            "stale from a previous reconcile",
		LastTransitionTime: metav1.Now(),
	}}
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return nil, &platform.CredentialError{Msg: "invalid credentials"}
		},
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("failed to get config: %v", err)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCompleteNodeInventory {
			t.Errorf("expected stale CompleteNodeInventory to be cleared, got %s/%s", cond.Status, cond.Reason)
		}
		if cond.Type == networkingv1alpha1.ConditionCloudResourcesReconciled {
			t.Errorf("expected stale CloudResourcesReconciled to be cleared, got %s/%s", cond.Status, cond.Reason)
		}
	}
}

// TestConfigReconcile_RouteServerNotFound_NoRequeue: non-existent route server ID is
// terminal — user must correct spec.aws.routeServerIDs.
func TestConfigReconcile_RouteServerNotFound_NoRequeue(t *testing.T) {
	mock := &mockPlatform{discoverErr: &awsplatform.RouteServerNotFoundError{ID: "rs-deadbeef"}}
	config := newTestCUDNBgpConfigWithAWS()
	config.Finalizers = []string{ConfigFinalizerName}
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return mock, nil
		},
	}

	result, _ := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for terminal RouteServerNotFound, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("failed to get config: %v", err)
	}
	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCloudEndpointsDiscovered {
			if cond.Reason != ReasonRouteServerNotFound {
				t.Errorf("expected reason RouteServerNotFound, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("CloudEndpointsDiscovered condition not found")
}

// TestConfigReconcile_AWSDiscovery_TransientStillRequeues: a generic AWS API error
// (not a missing route server) must remain transient and requeue after 30s.
func TestConfigReconcile_AWSDiscovery_TransientStillRequeues(t *testing.T) {
	mock := &mockPlatform{discoverErr: fmt.Errorf("ec2: request throttled")}
	config := newTestCUDNBgpConfigWithAWS()
	config.Finalizers = []string{ConfigFinalizerName}
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return mock, nil
		},
	}

	result, _ := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("generic AWS discovery failure must remain transient (30s), got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCloudEndpointsDiscovered {
			if cond.Reason != ReasonCloudDiscoveryFailed {
				t.Errorf("expected reason CloudDiscoveryFailed, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("CloudEndpointsDiscovered condition not found")
}

// Credentials that have been asked for and not yet minted are a wait,
// not a fault: CCO takes a few seconds, and reporting the operator
// Degraded in that window sends people looking for a problem that is
// about to solve itself.
func TestConfigReconcile_CredentialsPendingIsAWait(t *testing.T) {
	config := newTestCUDNBgpConfigWithAWS()
	config.Finalizers = []string{ConfigFinalizerName}
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return nil, fmt.Errorf("%w: ROLEARN must be set", platform.ErrCredentialsPending)
		},
	}

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if err != nil {
		t.Fatalf("expected no error while waiting, got %v", err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Errorf("expected a 10s requeue, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		// Every assertion below reads this object. Left unchecked, a failed
		// read leaves it zero and the loop over its conditions finds nothing
		// to assert, so the test passes without having looked at anything.
		t.Fatalf("reading back the CUDNBgpConfig: %v", err)
	}
	if updated.Status.Phase == networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected %s, got Degraded", networkingv1alpha1.PhaseConfiguring)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCloudEndpointsDiscovered {
			if cond.Status != metav1.ConditionFalse {
				t.Errorf("expected CloudEndpointsDiscovered=False, got %s", cond.Status)
			}
			if cond.Reason != "WaitingForCloudCredentials" {
				t.Errorf("expected reason WaitingForCloudCredentials, got %s", cond.Reason)
			}
			// The reason is a constant; the message is the only place the
			// cause reaches an administrator, so it has to carry what the
			// resolver said rather than a fixed sentence.
			if !strings.Contains(cond.Message, "ROLEARN must be set") {
				t.Errorf("the condition message discards what the resolver said: %q", cond.Message)
			}
			return
		}
	}
	t.Error("CloudEndpointsDiscovered condition not found")
}

func TestConfigReconcile_AWSReconcileFailure(t *testing.T) {
	mock := &mockPlatform{reconcileNodesErr: fmt.Errorf("ec2 API timeout")}
	config := newTestCUDNBgpConfigWithAWS()
	config.Finalizers = []string{ConfigFinalizerName}
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	node := newRouterNode("node-1", "10.0.1.10", "us-east-1a", "aws:///us-east-1a/i-001")

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod, node).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return mock, nil
		},
	}

	result, _ := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected 30s degraded requeue, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("failed to get config: %v", err)
	}
	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCloudResourcesReconciled {
			if cond.Reason != ReasonCloudReconcileFailed {
				t.Errorf("expected reason CloudReconcileFailed, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("CloudResourcesReconciled condition not found")
}

func TestConfigReconcile_AWSNodeFiltering(t *testing.T) {
	mock := &mockPlatform{}
	config := newTestCUDNBgpConfigWithAWS()
	config.Finalizers = []string{ConfigFinalizerName}
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	completeNodes := []*corev1.Node{
		newRouterNode("node-1", "10.0.1.10", "us-east-1a", "aws:///us-east-1a/i-001"),
		newRouterNode("node-2", "10.0.2.10", "us-east-1b", "aws:///us-east-1b/i-002"),
		newRouterNode("node-3", "10.0.3.10", "us-east-1c", "aws:///us-east-1c/i-003"),
	}
	missingIP := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-no-ip",
			Labels: map[string]string{"bgp_router": "true", "topology.kubernetes.io/zone": "us-east-1a"},
		},
		Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-004"},
	}
	missingAZ := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-no-az",
			Labels: map[string]string{"bgp_router": "true"},
		},
		Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-005"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.4.10"}},
		},
	}

	missingProviderID := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-no-provider",
			Labels: map[string]string{"bgp_router": "true", "topology.kubernetes.io/zone": "us-east-1a"},
		},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.5.10"}},
		},
	}

	objs := []client.Object{config, network, frrNS, frrPod, missingIP, missingAZ, missingProviderID}
	for _, n := range completeNodes {
		objs = append(objs, n)
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return mock, nil
		},
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if !mock.reconcileNodesCalled {
		t.Fatal("expected ReconcileNodes to be called")
	}
	if len(mock.reconcileNodesArgs) != 3 {
		t.Errorf("expected 3 nodes passed to ReconcileNodes, got %d", len(mock.reconcileNodesArgs))
	}
	wantNodes := map[string]bool{"node-1": true, "node-2": true, "node-3": true}
	for _, n := range mock.reconcileNodesArgs {
		if !wantNodes[n.Name] {
			t.Errorf("unexpected node passed to ReconcileNodes: %s", n.Name)
		}
		delete(wantNodes, n.Name)
	}
	if len(wantNodes) != 0 {
		t.Errorf("missing nodes in ReconcileNodes: %v", wantNodes)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("get updated config: %v", err)
	}
	if updated.Status.Phase != networkingv1alpha1.PhaseConfiguring {
		t.Errorf("expected Configuring, got %s", updated.Status.Phase)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCompleteNodeInventory {
			if cond.Status != metav1.ConditionFalse || cond.Reason != "NodesIncomplete" {
				t.Errorf("expected CompleteNodeInventory=False/NodesIncomplete, got %s/%s", cond.Status, cond.Reason)
			}
			if !strings.Contains(cond.Message, "node-no-ip") ||
				!strings.Contains(cond.Message, "node-no-az") ||
				!strings.Contains(cond.Message, "node-no-provider") {
				t.Errorf("expected message to list incomplete nodes, got %q", cond.Message)
			}
			return
		}
	}
	t.Error("CompleteNodeInventory condition not found")
}

func TestConfigReconcile_DeleteSucceedsWithCredentialFailure(t *testing.T) {
	now := metav1.Now()
	config := newTestCUDNBgpConfigWithAWS()
	config.Finalizers = []string{ConfigFinalizerName}
	config.DeletionTimestamp = &now

	s := configTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return nil, &platform.CredentialError{Msg: "invalid credentials"}
		},
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if err != nil {
		t.Fatalf("deletion should succeed even with credential failure, got: %v", err)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	for _, f := range updated.Finalizers {
		if f == ConfigFinalizerName {
			t.Error("finalizer should be removed even when AWS credentials are invalid")
		}
	}
}

func TestConfigReconcile_DeleteSuccessful(t *testing.T) {
	mock := &mockPlatform{}
	now := metav1.Now()
	config := newTestCUDNBgpConfigWithAWS()
	config.UID = testConfigUID
	config.Finalizers = []string{ConfigFinalizerName}
	config.DeletionTimestamp = &now

	frrObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "frrk8s.metallb.io/v1beta1",
			"kind":       "FRRConfiguration",
			"metadata": map[string]interface{}{
				"name":      "cudn-bgp-1",
				"namespace": FRRNamespace,
				"labels":    map[string]interface{}{LabelManagedBy: LabelManagedByVal},
			},
		},
	}
	withCUDNBgpConfigOwner(frrObj, config)

	s := configTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, frrObj).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return mock, nil
		},
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !mock.discoverCalled {
		t.Error("expected DiscoverEndpoints to be called before cleanup")
	}
	if !mock.cleanupCalled {
		t.Error("expected Cleanup to be called during deletion")
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cudn-bgp-1", Namespace: FRRNamespace}, obj); err == nil {
		t.Error("FRRConfiguration should be deleted during cleanup")
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	for _, f := range updated.Finalizers {
		if f == ConfigFinalizerName {
			t.Error("finalizer should be removed after successful deletion")
		}
	}
}

// TestDefaultPlatformBuilder_EveryEnumValueDispatches walks the enum itself
// rather than a list maintained beside it.
//
// A value added to the API and never given a builder is the failure this
// exists for: every other test injects a fake through PlatformBuilder and
// never reaches this switch, so a missing arm compiles, passes and then fails
// on a cluster with "no platform implementation".
//
// An error is expected here rather than a platform, because building one reads
// Infrastructure/cluster and the fake client has none. What must not happen is
// falling through to the default arm.
func TestDefaultPlatformBuilder_EveryEnumValueDispatches(t *testing.T) {
	for _, p := range networkingv1alpha1.AllPlatforms {
		if p == networkingv1alpha1.PlatformManual {
			continue // Manual builds no platform by design.
		}
		t.Run(string(p), func(t *testing.T) {
			config := newTestCUDNBgpConfigWithAWS()
			config.Spec.Platform = p
			c := fake.NewClientBuilder().WithScheme(configTestScheme()).Build()

			_, err := defaultPlatformBuilder(context.Background(), c, config)
			if err != nil && strings.Contains(err.Error(), "no platform implementation") {
				t.Errorf("%s reached the default arm: %v", p, err)
			}
		})
	}
}

// TestConfigReconcile_ManualSkipsPlatform pins that a Manual configuration
// constructs no cloud platform and reports none of the cloud conditions, so it
// needs no cloud credentials.
//
// The gate moved from "is spec.aws set" to "is the platform Manual", and that
// expression is the only thing standing between a Manual cluster and an
// attempt to reach a cloud API it has no business calling.
func TestConfigReconcile_ManualSkipsPlatform(t *testing.T) {
	config := newTestCUDNBgpConfig() // Platform: Manual
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	builderCalled := false
	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			builderCalled = true
			return &mockPlatform{}, nil
		},
	}

	_, _ = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	if builderCalled {
		t.Error("Manual must not construct a cloud platform")
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCloudEndpointsDiscovered ||
			cond.Type == networkingv1alpha1.ConditionCloudResourcesReconciled ||
			cond.Type == networkingv1alpha1.ConditionCompleteNodeInventory {
			t.Errorf("Manual must not report %s", cond.Type)
		}
	}
	if len(updated.Status.PeerGroups) != 0 {
		t.Errorf("Manual discovers nothing, so status.peerGroups should be empty, got %d", len(updated.Status.PeerGroups))
	}
}

func TestConfigReconcile_ManualClearsStaleCloudStatus(t *testing.T) {
	config := newTestCUDNBgpConfig()
	config.Status.PeerGroups = []networkingv1alpha1.PeerGroupStatus{
		{Key: "us-east-1a", Neighbors: []networkingv1alpha1.BGPNeighbor{{Address: "10.0.1.47", RemoteASN: 64512}}},
	}
	config.Status.Conditions = []metav1.Condition{
		{Type: networkingv1alpha1.ConditionCloudEndpointsDiscovered, Status: metav1.ConditionTrue, Reason: "Discovered"},
		{Type: networkingv1alpha1.ConditionCloudResourcesReconciled, Status: metav1.ConditionTrue, Reason: "Reconciled"},
		{Type: networkingv1alpha1.ConditionCompleteNodeInventory, Status: metav1.ConditionTrue, Reason: "Complete"},
	}
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("get updated config: %v", err)
	}
	if len(updated.Status.PeerGroups) != 0 {
		t.Errorf("expected stale peerGroups cleared, got %d", len(updated.Status.PeerGroups))
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionCloudEndpointsDiscovered ||
			cond.Type == networkingv1alpha1.ConditionCloudResourcesReconciled ||
			cond.Type == networkingv1alpha1.ConditionCompleteNodeInventory {
			t.Errorf("expected stale %s removed", cond.Type)
		}
	}
}

// TestConfigReconcile_CloudSteadyStateDoesNotRewriteStatus guards the hot loop
// that clearing cloud conditions before every platform re-eval would cause: the
// conditions get re-added with a fresh LastTransitionTime, the status write
// differs from etcd, and (with no generation predicate) the write re-enqueues
// the singleton forever. A steady-state reconcile must not write status.
func TestConfigReconcile_CloudSteadyStateDoesNotRewriteStatus(t *testing.T) {
	mock := &mockPlatform{}
	config := newTestCUDNBgpConfigWithAWS()
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	node := newRouterNode("node-1", "10.0.1.10", "us-east-1a", "aws:///us-east-1a/i-001")

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod, node).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return mock, nil
		},
	}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}

	// Drive to steady state: finalizer add, then a full reconcile that writes status.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	settled := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), req.NamespacedName, settled); err != nil {
		t.Fatalf("get after settle: %v", err)
	}
	if settled.Status.Phase != networkingv1alpha1.PhaseReady {
		t.Fatalf("expected Ready before steady-state check, got %s", settled.Status.Phase)
	}
	rvBefore := settled.ResourceVersion

	// A further reconcile with nothing changed must not write status.
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("steady-state reconcile: %v", err)
	}
	after := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), req.NamespacedName, after); err != nil {
		t.Fatalf("get after steady-state reconcile: %v", err)
	}
	if after.ResourceVersion != rvBefore {
		t.Errorf("steady-state reconcile rewrote status: resourceVersion %s -> %s", rvBefore, after.ResourceVersion)
	}
}

// TestConfigReconcile_CredentialFailureClearsStalePeerGroups verifies that a
// credential failure clears the cloud peering plan reported from a prior
// success, rather than leaving stale endpoints alongside the degraded status.
func TestConfigReconcile_CredentialFailureClearsStalePeerGroups(t *testing.T) {
	config := newTestCUDNBgpConfigWithAWS()
	config.Finalizers = []string{ConfigFinalizerName}
	// Stale plan and cloud conditions from an earlier successful reconcile.
	config.Status.PeerGroups = []networkingv1alpha1.PeerGroupStatus{
		{Key: "us-east-1a", Neighbors: []networkingv1alpha1.BGPNeighbor{{Address: "10.0.1.47", RemoteASN: 64512}}},
	}
	config.Status.Conditions = []metav1.Condition{
		{Type: networkingv1alpha1.ConditionCloudEndpointsDiscovered, Status: metav1.ConditionTrue, Reason: "Discovered"},
	}
	s := configTestScheme()

	network := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "operator.openshift.io/v1",
			"kind":       "Network",
			"metadata":   map[string]interface{}{"name": "cluster"},
			"spec":       map[string]interface{}{},
		},
	}
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, network, frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{
		Client: c, Scheme: s,
		PlatformBuilder: func(_ context.Context, _ client.Client, _ *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
			return nil, &platform.CredentialError{Msg: "invalid credentials"}
		},
	}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("get config: %v", err)
	}
	if len(updated.Status.PeerGroups) != 0 {
		t.Errorf("expected stale peerGroups cleared on credential failure, got %d", len(updated.Status.PeerGroups))
	}
	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
	got := meta.FindStatusCondition(updated.Status.Conditions, networkingv1alpha1.ConditionCloudEndpointsDiscovered)
	if got == nil || got.Status != metav1.ConditionFalse || got.Reason != ReasonCloudCredentialsInvalid {
		t.Errorf("CloudEndpointsDiscovered = %+v, want False/%s", got, ReasonCloudCredentialsInvalid)
	}
}

// TestDefaultPlatformBuilder_UnknownPlatform pins what the default arm is for.
func TestDefaultPlatformBuilder_UnknownPlatform(t *testing.T) {
	config := newTestCUDNBgpConfigWithAWS()
	config.Spec.Platform = networkingv1alpha1.PlatformType("Nowhere")
	c := fake.NewClientBuilder().WithScheme(configTestScheme()).Build()

	_, err := defaultPlatformBuilder(context.Background(), c, config)
	if err == nil || !strings.Contains(err.Error(), "no platform implementation") {
		t.Errorf("expected an unknown platform to be refused, got %v", err)
	}
}

// --- Network patch ownership tracking (Phase 1) ---

func TestConfigReconcile_SetsNetworkOwnershipWhenNotPreExisting(t *testing.T) {
	config := newTestCUDNBgpConfig()
	s := configTestScheme()
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, newEmptyNetwork(), frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("failed to get config: %v", err)
	}
	if updated.Status.FRRProviderOwnership != networkingv1alpha1.NetworkPatchOwnershipOwned {
		t.Error("expected FRRProviderOwnership=Owned when FRR was not active before first patch")
	}
	if updated.Status.RouteAdvertisementsOwnership != networkingv1alpha1.NetworkPatchOwnershipOwned {
		t.Error("expected RouteAdvertisementsOwnership=Owned when routeAdvertisements was not Enabled before first patch")
	}
}

func TestConfigReconcile_DoesNotPersistOwnedWhenNetworkPatchFails(t *testing.T) {
	config := newTestCUDNBgpConfig()
	s := configTestScheme()
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, newEmptyNetwork(), frrNS, frrPod).
		WithStatusSubresource(config).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetObjectKind().GroupVersionKind() == NetworkGVK {
					return fmt.Errorf("injected patch failure")
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("failed to get config: %v", err)
	}
	if updated.Status.FRRProviderOwnership != "" {
		t.Errorf("FRRProviderOwnership=%q, want empty when Network patch failed", updated.Status.FRRProviderOwnership)
	}
	if updated.Status.RouteAdvertisementsOwnership != "" {
		t.Errorf("RouteAdvertisementsOwnership=%q, want empty when Network patch failed", updated.Status.RouteAdvertisementsOwnership)
	}
}

func TestConfigReconcile_SetsExternalOwnershipWhenFRRPreExisting(t *testing.T) {
	// Network/cluster already has FRR before the operator starts (External ownership).
	config := newTestCUDNBgpConfig()
	s := configTestScheme()
	frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
	frrPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, newFRREnabledNetwork(), frrNS, frrPod).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}
	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("failed to get config: %v", err)
	}
	if updated.Status.FRRProviderOwnership != networkingv1alpha1.NetworkPatchOwnershipExternal {
		t.Errorf("expected FRRProviderOwnership=External when FRR was already active, got %q", updated.Status.FRRProviderOwnership)
	}
	if updated.Status.RouteAdvertisementsOwnership != networkingv1alpha1.NetworkPatchOwnershipExternal {
		t.Errorf("expected RouteAdvertisementsOwnership=External when routeAdvertisements was already Enabled, got %q", updated.Status.RouteAdvertisementsOwnership)
	}
}

func TestConfigReconcile_PartialNetworkOwnership(t *testing.T) {
	cases := []struct {
		name                             string
		network                          *unstructured.Unstructured
		wantFRRProviderOwnership         networkingv1alpha1.NetworkPatchOwnership
		wantRouteAdvertisementsOwnership networkingv1alpha1.NetworkPatchOwnership
	}{
		{
			name: "FRR already in providers, claim only route ads",
			network: newNetworkObject(map[string]interface{}{
				"additionalRoutingCapabilities": map[string]interface{}{
					"providers": []interface{}{FRRProviderName},
				},
			}),
			wantFRRProviderOwnership:         networkingv1alpha1.NetworkPatchOwnershipExternal,
			wantRouteAdvertisementsOwnership: networkingv1alpha1.NetworkPatchOwnershipOwned,
		},
		{
			name: "route ads already Enabled, claim only FRR",
			network: newNetworkObject(map[string]interface{}{
				"defaultNetwork": map[string]interface{}{
					"ovnKubernetesConfig": map[string]interface{}{
						"routeAdvertisements": RouteAdvertisementsOn,
					},
				},
			}),
			wantFRRProviderOwnership:         networkingv1alpha1.NetworkPatchOwnershipOwned,
			wantRouteAdvertisementsOwnership: networkingv1alpha1.NetworkPatchOwnershipExternal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config := newTestCUDNBgpConfig()
			s := configTestScheme()
			frrNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: FRRNamespace}}
			frrPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "frr-k8s-pod", Namespace: FRRNamespace, Labels: map[string]string{"app": "frr-k8s"}},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			}
			c := fake.NewClientBuilder().WithScheme(s).
				WithObjects(config, tc.network, frrNS, frrPod).
				WithStatusSubresource(config).
				Build()
			r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				t.Fatalf("first reconcile: %v", err)
			}
			if _, err := r.Reconcile(context.Background(), req); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			updated := &networkingv1alpha1.CUDNBgpConfig{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
				t.Fatalf("get config: %v", err)
			}
			if updated.Status.FRRProviderOwnership != tc.wantFRRProviderOwnership {
				t.Errorf("FRRProviderOwnership=%q, want %q", updated.Status.FRRProviderOwnership, tc.wantFRRProviderOwnership)
			}
			if updated.Status.RouteAdvertisementsOwnership != tc.wantRouteAdvertisementsOwnership {
				t.Errorf("RouteAdvertisementsOwnership=%q, want %q", updated.Status.RouteAdvertisementsOwnership, tc.wantRouteAdvertisementsOwnership)
			}
			gotNetwork := &unstructured.Unstructured{}
			gotNetwork.SetGroupVersionKind(NetworkGVK)
			if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, gotNetwork); err != nil {
				t.Fatalf("get Network: %v", err)
			}
			if tc.wantRouteAdvertisementsOwnership == networkingv1alpha1.NetworkPatchOwnershipOwned && mustNetworkRouteAds(t, gotNetwork) != RouteAdvertisementsOn {
				t.Error("expected routeAdvertisements Enabled after claiming RouteAdvertisementsOwnership=Owned")
			}
			if tc.wantFRRProviderOwnership == networkingv1alpha1.NetworkPatchOwnershipOwned {
				hasFRR := false
				for _, p := range mustNetworkProviders(t, gotNetwork) {
					if p == FRRProviderName {
						hasFRR = true
					}
				}
				if !hasFRR {
					t.Error("expected FRR in providers after claiming FRRProviderOwnership=Owned")
				}
			}
		})
	}
}

// --- reconcileDelete: Network unpatch ---

func TestConfigReconcile_DeleteUnpatchesNetworkWhenOwned(t *testing.T) {
	now := metav1.Now()
	config := newTestCUDNBgpConfig()
	config.UID = testConfigUID
	config.Finalizers = []string{ConfigFinalizerName}
	config.DeletionTimestamp = &now
	config.Status.FRRProviderOwnership = networkingv1alpha1.NetworkPatchOwnershipOwned
	config.Status.RouteAdvertisementsOwnership = networkingv1alpha1.NetworkPatchOwnershipOwned

	managedFRR := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1", "kind": "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name":       "cudn-bgp-1",
			"namespace":  FRRNamespace,
			"labels":     map[string]interface{}{LabelManagedBy: LabelManagedByVal},
			"finalizers": []interface{}{"frrk8s.metallb.io/finalizer"},
		},
	}}
	withCUDNBgpConfigOwner(managedFRR, config)

	s := configTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, newFRREnabledNetwork(), managedFRR).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	frrObj := &unstructured.Unstructured{}
	frrObj.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cudn-bgp-1", Namespace: FRRNamespace}, frrObj); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Fatalf("failed to get managed FRRConfiguration: %v", err)
		}
	} else if frrObj.GetDeletionTimestamp().IsZero() {
		t.Error("managed FRRConfiguration should be deleted or terminating")
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Fatalf("failed to get config after delete: %v", err)
		}
	} else {
		for _, f := range updated.Finalizers {
			if f == ConfigFinalizerName {
				t.Error("finalizer should be removed after delete")
			}
		}
	}

	network := &unstructured.Unstructured{}
	network.SetGroupVersionKind(NetworkGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, network); err != nil {
		t.Fatalf("failed to read Network: %v", err)
	}
	if networkHasFRRProvider(t, network) {
		t.Error("FRR should be removed from Network/cluster after delete")
	}
	if got := mustNetworkRouteAds(t, network); got != RouteAdvertisementsDisabled {
		t.Errorf("routeAdvertisements = %q, want %q after delete", got, RouteAdvertisementsDisabled)
	}
}

// TestConfigReconcile_DeleteRemovesFinalizerWhenUnpatchFails asserts that a
// failure reverting the Network/cluster patch is best-effort: it must not wedge
// the finalizer and leave the CUDNBgpConfig stuck Terminating forever.
func TestConfigReconcile_DeleteRemovesFinalizerWhenUnpatchFails(t *testing.T) {
	now := metav1.Now()
	config := newTestCUDNBgpConfig()
	config.Finalizers = []string{ConfigFinalizerName}
	config.DeletionTimestamp = &now
	config.Status.FRRProviderOwnership = networkingv1alpha1.NetworkPatchOwnershipOwned
	config.Status.RouteAdvertisementsOwnership = networkingv1alpha1.NetworkPatchOwnershipOwned

	s := configTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, newFRREnabledNetwork()).
		WithStatusSubresource(config).
		WithInterceptorFuncs(interceptor.Funcs{
			// Fail the Network/cluster revert patch to simulate a webhook rejection
			// or transient API error during unpatch.
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetObjectKind().GroupVersionKind() == NetworkGVK {
					return fmt.Errorf("injected patch failure")
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
		t.Fatalf("reconcileDelete must not return an error when unpatch fails, got: %v", err)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		if apierrors.IsNotFound(err) {
			return // config fully deleted once finalizer was removed — acceptable
		}
		t.Fatalf("failed to get config after delete: %v", err)
	}
	for _, f := range updated.Finalizers {
		if f == ConfigFinalizerName {
			t.Error("finalizer must be removed even when Network unpatch fails, to avoid a stuck deletion")
		}
	}
}

func TestConfigReconcile_DeleteSkipsUnpatchWhenNotOwned(t *testing.T) {
	// FRR was pre-existing; delete must not unpatch Network/cluster.
	now := metav1.Now()
	config := newTestCUDNBgpConfig()
	config.Finalizers = []string{ConfigFinalizerName}
	config.DeletionTimestamp = &now

	s := configTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, newFRREnabledNetwork()).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}}); err != nil {
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
		t.Error("Network should not be unpatched when FRRProviderOwnership is not Owned")
	}
	if mustNetworkRouteAds(t, network) != RouteAdvertisementsOn {
		t.Error("routeAdvertisements should remain Enabled when RouteAdvertisementsOwnership is not Owned")
	}
}

func TestConfigReconcile_DeleteSkipsUnpatchWhenExternalFRRConfigExists(t *testing.T) {
	now := metav1.Now()
	config := newTestCUDNBgpConfig()
	config.UID = testConfigUID
	config.Finalizers = []string{ConfigFinalizerName}
	config.DeletionTimestamp = &now
	config.Status.FRRProviderOwnership = networkingv1alpha1.NetworkPatchOwnershipOwned
	config.Status.RouteAdvertisementsOwnership = networkingv1alpha1.NetworkPatchOwnershipOwned

	externalFRR := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1", "kind": "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name": "external-frr", "namespace": FRRNamespace,
			"labels": map[string]interface{}{"owner": "metallb"},
		},
	}}
	managedFRR := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1", "kind": "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name": "cudn-bgp-1", "namespace": FRRNamespace,
			"labels": map[string]interface{}{LabelManagedBy: LabelManagedByVal},
		},
	}}
	withCUDNBgpConfigOwner(managedFRR, config)

	s := configTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(config, newFRREnabledNetwork(), externalFRR, managedFRR).
		WithStatusSubresource(config).
		Build()

	r := &CUDNBgpConfigReconciler{Client: c, Scheme: s}
	res, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("deletion should requeue while an external FRRConfiguration still exists")
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
		t.Error("Network should not be unpatched when external FRRConfiguration still exists")
	}
	if mustNetworkRouteAds(t, network) != RouteAdvertisementsOn {
		t.Error("routeAdvertisements should remain Enabled when external FRRConfiguration still exists")
	}

	// The finalizer must be retained so ownership survives until the external
	// consumer is gone; otherwise nothing could ever revert the Network patch.
	updated := &networkingv1alpha1.CUDNBgpConfig{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated); err != nil {
		t.Fatalf("CUDNBgpConfig should still exist (finalizer retained): %v", err)
	}
	if !slices.Contains(updated.Finalizers, ConfigFinalizerName) {
		t.Error("finalizer should be retained while an external FRRConfiguration still exists")
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, ConditionDeletionBlocked)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != ReasonExternalFRRConfigsExist {
		t.Errorf("expected DeletionBlocked=True/%s condition, got %+v", ReasonExternalFRRConfigsExist, cond)
	}
}
