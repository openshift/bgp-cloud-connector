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
			BGP: networkingv1alpha1.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingv1alpha1.LivenessDetectionBGPKeepalive,
				AvailabilityZones: []networkingv1alpha1.AvailabilityZone{
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
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cudn-bgp-az-1", Namespace: FRRNamespace}, frrConfig); err != nil {
		t.Fatalf("FRRConfiguration not created: %v", err)
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
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
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
		RouteServers: []platform.DiscoveredRouteServer{
			{
				RouteServerID: "rs-1",
				RemoteASN:     64512,
				Endpoints: []platform.DiscoveredEndpoint{
					{EndpointID: "rse-001", AZ: "us-east-1a", Address: "10.0.1.47"},
				},
			},
		},
		NeighborsByAZ: map[string][]platform.DiscoveredNeighbor{
			"us-east-1a": {{Address: "10.0.1.47", ASN: 64512}},
		},
		EndpointsByAZ: map[string][]string{
			"us-east-1a": {"rse-001"},
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
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	if updated.Status.Phase != networkingv1alpha1.PhaseReady {
		t.Errorf("expected Ready, got %s", updated.Status.Phase)
	}
	if len(updated.Status.Conditions) != 6 {
		t.Errorf("expected 6 conditions, got %d", len(updated.Status.Conditions))
	}
	if updated.Status.AWS == nil {
		t.Fatal("expected status.aws to be populated")
	}
	if len(updated.Status.AWS.RouteServers) != 1 {
		t.Errorf("expected 1 route server in status, got %d", len(updated.Status.AWS.RouteServers))
	}

	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionAWSEndpointsDiscovered {
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("expected AWSEndpointsDiscovered=True, got %s", cond.Status)
			}
		}
		if cond.Type == networkingv1alpha1.ConditionAWSResourcesReconciled {
			if cond.Status != metav1.ConditionTrue {
				t.Errorf("expected AWSResourcesReconciled=True, got %s", cond.Status)
			}
		}
		if cond.Type == networkingv1alpha1.ConditionIncompleteNodeInventory {
			if cond.Status != metav1.ConditionTrue || cond.Reason != "Complete" {
				t.Errorf("expected IncompleteNodeInventory=True/Complete, got %s/%s", cond.Status, cond.Reason)
			}
		}
	}

	// Verify FRR was created from discovery
	frrConfig := &unstructured.Unstructured{}
	frrConfig.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cudn-bgp-az-1", Namespace: FRRNamespace}, frrConfig); err != nil {
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
			return nil, &awsplatform.CredentialError{Msg: "invalid credentials"}
		},
	}

	result, _ := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "cluster"}})
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected 30s degraded requeue, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionAWSEndpointsDiscovered {
			if cond.Reason != "AWSCredentialsInvalid" {
				t.Errorf("expected reason AWSCredentialsInvalid, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("AWSEndpointsDiscovered condition not found")
}

func TestConfigReconcile_AWSDiscoveryFailure(t *testing.T) {
	mock := &mockPlatform{discoverErr: fmt.Errorf("DescribeRouteServers: InvalidRouteServerID")}
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
		t.Errorf("expected 30s degraded requeue, got %v", result.RequeueAfter)
	}

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionAWSEndpointsDiscovered {
			if cond.Reason != "AWSDiscoveryFailed" {
				t.Errorf("expected reason AWSDiscoveryFailed, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("AWSEndpointsDiscovered condition not found")
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
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	if updated.Status.Phase != networkingv1alpha1.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionAWSResourcesReconciled {
			if cond.Reason != "AWSReconcileFailed" {
				t.Errorf("expected reason AWSReconcileFailed, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("AWSResourcesReconciled condition not found")
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

	updated := &networkingv1alpha1.CUDNBgpConfig{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "cluster"}, updated)
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingv1alpha1.ConditionIncompleteNodeInventory {
			if cond.Reason != "NodesIncomplete" {
				t.Errorf("expected reason NodesIncomplete, got %s", cond.Reason)
			}
			if !strings.Contains(cond.Message, "node-no-ip") ||
				!strings.Contains(cond.Message, "node-no-az") ||
				!strings.Contains(cond.Message, "node-no-provider") {
				t.Errorf("expected message to list incomplete nodes, got %q", cond.Message)
			}
			return
		}
	}
	t.Error("IncompleteNodeInventory condition not found")
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
			return nil, &awsplatform.CredentialError{Msg: "invalid credentials"}
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
				"name":      "cudn-bgp-az-1",
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
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cudn-bgp-az-1", Namespace: FRRNamespace}, obj); err == nil {
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
