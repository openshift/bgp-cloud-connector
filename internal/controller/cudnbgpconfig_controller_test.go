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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
	discoverResult       *platform.DiscoveryResult
	discoverErr          error
	discoverCalled       bool
	reconcileNodesCalled bool
	reconcileNodesArgs   []platform.RouterNode
	reconcileNodesErr    error
	cleanupCalled        bool
	cleanupErr           error
}

func (m *mockPlatform) DiscoverEndpoints(_ context.Context) (*platform.DiscoveryResult, error) {
	m.discoverCalled = true
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
	if len(updated.Status.Conditions) != 5 {
		t.Errorf("expected 5 conditions, got %d", len(updated.Status.Conditions))
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
	}

	// Verify FRR was created from discovery
	frrConfig := &unstructured.Unstructured{}
	frrConfig.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cudn-bgp-1", Namespace: FRRNamespace}, frrConfig); err != nil {
		t.Fatalf("FRRConfiguration not created from discovery: %v", err)
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

	objs := []client.Object{config, network, frrNS, frrPod, missingIP, missingAZ}
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
			cond.Type == networkingv1alpha1.ConditionCloudResourcesReconciled {
			t.Errorf("Manual must not report %s", cond.Type)
		}
	}
	if len(updated.Status.PeerGroups) != 0 {
		t.Errorf("Manual discovers nothing, so status.peerGroups should be empty, got %d", len(updated.Status.PeerGroups))
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
