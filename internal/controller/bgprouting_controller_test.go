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
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

func routingTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		panic(err)
	}
	if err := networkingapi.AddToScheme(s); err != nil {
		panic(err)
	}

	s.AddKnownTypeWithName(ClusterUDNGVK.GroupVersion().WithKind("ClusterUserDefinedNetwork"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(ClusterUDNGVK.GroupVersion().WithKind("ClusterUserDefinedNetworkList"), &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(RouteAdvertisementsGVK.GroupVersion().WithKind("RouteAdvertisements"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(RouteAdvertisementsGVK.GroupVersion().WithKind("RouteAdvertisementsList"), &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(VirtualMachineInstanceGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(VirtualMachineInstanceGVK.GroupVersion().WithKind("VirtualMachineInstanceList"), &unstructured.UnstructuredList{})
	return s
}

func newTestBGPRouting() *networkingapi.BGPRouting {
	return &networkingapi.BGPRouting{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prod",
			UID:  "6d2f5b1e-0f5a-4a1e-9f4c-0b7d2a5c1e33",
		},
		Spec: networkingapi.BGPRoutingSpec{
			Network: networkingapi.NetworkConfig{
				Name:    "prod",
				Subnets: []string{"10.100.0.0/16"},
			},
		},
	}
}

func newReadyBGPCloudConfiguration() *networkingapi.BGPCloudConfiguration {
	return &networkingapi.BGPCloudConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
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
		Status: networkingapi.BGPCloudConfigurationStatus{
			Phase: networkingapi.PhaseReady,
		},
	}
}

func TestRoutingReconcile_FullReconcile(t *testing.T) {
	routing := newTestBGPRouting()
	config := newReadyBGPCloudConfiguration()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app1",
			Labels: map[string]string{
				LabelPrimaryUDN: "",
				LabelClusterUDN: "prod",
			},
		},
	}

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routing, config, ns).
		WithStatusSubresource(routing, config).
		Build()

	r := &BGPRoutingReconciler{Client: c, Scheme: s}

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

	updated := &networkingapi.BGPRouting{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "prod"}, updated); err != nil {
		t.Fatalf("failed to get updated BGPRouting: %v", err)
	}
	if updated.Status.Phase != networkingapi.PhaseReady {
		t.Errorf("expected Ready, got %s", updated.Status.Phase)
	}
	if len(updated.Status.Conditions) != 3 {
		t.Errorf("expected 3 conditions, got %d", len(updated.Status.Conditions))
	}

	// Verify ClusterUDN created
	cudn := &unstructured.Unstructured{}
	cudn.SetGroupVersionKind(ClusterUDNGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: "cluster-udn-prod"}, cudn); err != nil {
		t.Fatalf("ClusterUDN not created: %v", err)
	}

	// Verify RouteAdvertisements created
	ra := &unstructured.Unstructured{}
	ra.SetGroupVersionKind(RouteAdvertisementsGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: RouteAdvertisementName}, ra); err != nil {
		t.Fatalf("RouteAdvertisements not created: %v", err)
	}
	expressions, _, _ := unstructured.NestedSlice(ra.Object, "spec", "frrConfigurationSelector", "matchExpressions")
	if len(expressions) != 1 {
		t.Fatalf("FRR selector expressions = %v, want one expression", expressions)
	}
	expression, ok := expressions[0].(map[string]interface{})
	if !ok {
		t.Fatalf("FRR selector expression has type %T, want map", expressions[0])
	}
	if expression["key"] != LabelManagedBy || expression["operator"] != "NotIn" {
		t.Errorf("FRR selector expression = %v, want managed-by NotIn", expression)
	}
	values, ok := expression["values"].([]interface{})
	if !ok || len(values) != 1 || values[0] != LabelManagedByVMHostRoutes {
		t.Errorf("FRR selector values = %v, want [%s]", expression["values"], LabelManagedByVMHostRoutes)
	}
}

func TestRoutingReconcile_StaysReadyWhileVMIAddressIsPending(t *testing.T) {
	routing := newTestBGPRouting()
	routing.Finalizers = []string{RoutingFinalizerName}
	config := newReadyBGPCloudConfiguration()
	config.Spec.BGP.PeerGroups[0].NodeSelector = nil
	node := testRouterNode("worker-a", "a")
	ns := testVMNamespace()
	vmi := testVMI("vms", "vm", node.Name)

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routing, config, node, ns, vmi).
		WithStatusSubresource(routing, config).
		Build()
	r := &BGPRoutingReconciler{Client: c, Scheme: s}

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: routing.Name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAfter != 5*time.Second {
		t.Fatalf("requeue = %v, want 5s", result.RequeueAfter)
	}
	updated := &networkingapi.BGPRouting{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(routing), updated); err != nil {
		t.Fatalf("get BGPRouting: %v", err)
	}
	if updated.Status.Phase != networkingapi.PhaseReady {
		t.Fatalf("phase = %s, want Ready", updated.Status.Phase)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, networkingapi.ConditionVMHostRoutesConfigured)
	if condition == nil || condition.Status != metav1.ConditionUnknown || condition.Reason != ReasonWaitingForVMIPs {
		t.Fatalf("VM host-route condition = %#v, want Unknown/%s", condition, ReasonWaitingForVMIPs)
	}
}

// A VM on a node outside the router pool cannot be given a host route, but the
// ClusterUDN and the RouteAdvertisements are configured, so the network is
// Ready and the condition carries the shortfall.
func TestRoutingReconcile_StaysReadyWhenAVMCannotBeServed(t *testing.T) {
	routing := newTestBGPRouting()
	routing.Finalizers = []string{RoutingFinalizerName}
	config := newReadyBGPCloudConfiguration()
	config.Spec.BGP.PeerGroups[0].NodeSelector = nil
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "worker-plain",
		Labels: map[string]string{corev1.LabelHostname: "worker-plain"},
	}}
	ns := testVMNamespace()
	vmi := testVMI("vms", "vm", node.Name, "10.100.0.4")

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routing, config, node, ns, vmi).
		WithStatusSubresource(routing, config).
		Build()
	r := &BGPRoutingReconciler{Client: c, Scheme: s}

	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: routing.Name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updated := &networkingapi.BGPRouting{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(routing), updated); err != nil {
		t.Fatalf("get BGPRouting: %v", err)
	}
	if updated.Status.Phase != networkingapi.PhaseReady {
		t.Fatalf("phase = %s, want Ready", updated.Status.Phase)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, networkingapi.ConditionVMHostRoutesConfigured)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != ReasonVMHostRoutesIncomplete {
		t.Fatalf("VM host-route condition = %#v, want False/%s", condition, ReasonVMHostRoutesIncomplete)
	}
	if !strings.Contains(condition.Message, node.Name) {
		t.Fatalf("condition message %q does not name %s", condition.Message, node.Name)
	}
	for _, other := range []string{networkingapi.ConditionNetworkCreated, networkingapi.ConditionRouteAdvertisementsCreated} {
		if c := meta.FindStatusCondition(updated.Status.Conditions, other); c == nil || c.Status != metav1.ConditionTrue {
			t.Fatalf("%s = %#v, want True", other, c)
		}
	}
}

func TestMapWorkloadToRoutingSelectsOnlyNamespaceNetwork(t *testing.T) {
	prod := newTestBGPRouting()
	staging := newTestBGPRouting()
	staging.Name = "staging"
	staging.Spec.Network.Name = "staging"
	namespace := testVMNamespace()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "virt-launcher", Namespace: namespace.Name}}

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(prod, staging, namespace).Build()
	r := &BGPRoutingReconciler{Client: c, Scheme: s}

	requests := r.mapWorkloadToRouting(context.Background(), pod)
	if len(requests) != 1 || requests[0].Name != prod.Name {
		t.Fatalf("requests = %v, want only %q", requests, prod.Name)
	}
}

func TestNamespaceRemovalWithdrawsVMHostRoutes(t *testing.T) {
	ctx := context.Background()
	routing := newTestBGPRouting()
	routing.Finalizers = []string{RoutingFinalizerName}
	config := newReadyBGPCloudConfiguration()
	config.Spec.BGP.PeerGroups[0].NodeSelector = nil
	namespace := testVMNamespace()
	node := testRouterNode("worker-a", "a")
	vmi := testVMI(namespace.Name, "vm", node.Name, "10.100.0.4")
	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routing, config, namespace, node, vmi).
		WithStatusSubresource(routing, config).Build()
	r := &BGPRoutingReconciler{Client: c, Scheme: s}
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(routing)}
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	routes := &unstructured.UnstructuredList{}
	routes.SetGroupVersionKind(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"))
	if err := c.List(ctx, routes, client.InNamespace(FRRNamespace), client.MatchingLabels{LabelManagedBy: LabelManagedByVMHostRoutes}); err != nil {
		t.Fatalf("list VM host routes: %v", err)
	}
	if len(routes.Items) != 1 {
		t.Fatalf("VM host-route FRRConfigurations = %d, want 1", len(routes.Items))
	}

	// Route withdrawal must not depend on BGPCloudConfiguration readiness.
	config.Status.Phase = networkingapi.PhasePending
	if err := c.Status().Update(ctx, config); err != nil {
		t.Fatalf("mark BGPCloudConfiguration pending: %v", err)
	}
	if err := c.Delete(ctx, namespace); err != nil {
		t.Fatalf("delete namespace: %v", err)
	}
	requests := r.mapNamespaceToRouting(ctx, namespace)
	if len(requests) != 1 || requests[0] != request {
		t.Fatalf("namespace delete requests = %v, want %v", requests, request)
	}
	result, err := r.Reconcile(ctx, requests[0])
	if err != nil {
		t.Fatalf("reconcile after namespace deletion: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("requeue = %v, want 30s", result.RequeueAfter)
	}
	if err := c.List(ctx, routes, client.InNamespace(FRRNamespace), client.MatchingLabels{LabelManagedBy: LabelManagedByVMHostRoutes}); err != nil {
		t.Fatalf("list VM host routes after deletion: %v", err)
	}
	if len(routes.Items) != 0 {
		t.Fatalf("VM host-route FRRConfigurations after namespace deletion = %d, want 0", len(routes.Items))
	}
	updated := &networkingapi.BGPRouting{}
	if err := c.Get(ctx, request.NamespacedName, updated); err != nil {
		t.Fatalf("get BGPRouting: %v", err)
	}
	if updated.Status.Phase != networkingapi.PhaseDegraded {
		t.Fatalf("phase = %s, want Degraded", updated.Status.Phase)
	}
	networkCondition := meta.FindStatusCondition(updated.Status.Conditions, networkingapi.ConditionNetworkCreated)
	if networkCondition == nil || networkCondition.Reason != ReasonNamespaceNotReady {
		t.Fatalf("network condition = %#v, want %s", networkCondition, ReasonNamespaceNotReady)
	}
	routeCondition := meta.FindStatusCondition(updated.Status.Conditions, networkingapi.ConditionVMHostRoutesConfigured)
	if routeCondition == nil || routeCondition.Status != metav1.ConditionTrue || routeCondition.Reason != ReasonReconciled ||
		routeCondition.Message != "Configured 0 VM host routes" {
		t.Fatalf("VM host-route condition = %#v, want True/%s with zero routes", routeCondition, ReasonReconciled)
	}
}

func TestNamespaceLabelChangePredicate(t *testing.T) {
	old := testVMNamespace()
	newNamespace := old.DeepCopy()
	newNamespace.Labels["unrelated"] = "changed"
	pred := namespaceLabelChangePredicate()
	if pred.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newNamespace}) {
		t.Fatal("unrelated label change triggered reconciliation")
	}
	delete(newNamespace.Labels, LabelPrimaryUDN)
	if !pred.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: newNamespace}) {
		t.Fatal("removing the empty primary network label did not trigger reconciliation")
	}
}

func TestNamespaceListErrorPreservesVMHostRoutes(t *testing.T) {
	ctx := context.Background()
	routing := newTestBGPRouting()
	routing.Finalizers = []string{RoutingFinalizerName}
	config := newReadyBGPCloudConfiguration()
	stale := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1", "kind": "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name": "existing-route", "namespace": FRRNamespace,
			"labels":      map[string]interface{}{LabelManagedBy: LabelManagedByVMHostRoutes},
			"annotations": map[string]interface{}{AnnotationBGPRouting: routing.Name},
		},
	}}
	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routing, config, stale).
		WithStatusSubresource(routing, config).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.NamespaceList); ok {
					return errors.New("namespace API unavailable")
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	r := &BGPRoutingReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(routing)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(stale), got); err != nil {
		t.Fatalf("VM host route removed on namespace list error: %v", err)
	}
}

func TestWorkloadWatchObjectBeforeAndAfterKubeVirtInstall(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{VirtualMachineInstanceGVK.GroupVersion()})
	withoutKubeVirt, err := workloadWatchObject(mapper)
	if err != nil {
		t.Fatalf("choose fallback watch: %v", err)
	}
	if _, ok := withoutKubeVirt.(*corev1.Pod); !ok {
		t.Fatalf("watch without KubeVirt = %T, want Pod", withoutKubeVirt)
	}
	mapper.AddSpecific(VirtualMachineInstanceGVK,
		VirtualMachineInstanceGVK.GroupVersion().WithResource("virtualmachineinstances"),
		VirtualMachineInstanceGVK.GroupVersion().WithResource("virtualmachineinstance"), meta.RESTScopeNamespace)
	withKubeVirt, err := workloadWatchObject(mapper)
	if err != nil {
		t.Fatalf("choose VMI watch: %v", err)
	}
	if _, ok := withKubeVirt.(*unstructured.Unstructured); !ok || withKubeVirt.GetObjectKind().GroupVersionKind() != VirtualMachineInstanceGVK {
		t.Fatalf("watch with KubeVirt = %T %s, want VMI", withKubeVirt, withKubeVirt.GetObjectKind().GroupVersionKind())
	}
}

func TestRoutingReconcile_VMIAPILossKeepsNetworkReady(t *testing.T) {
	routing := newTestBGPRouting()
	routing.Finalizers = []string{RoutingFinalizerName}
	config := newReadyBGPCloudConfiguration()
	stale := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1", "kind": "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name": "existing-route", "namespace": FRRNamespace,
			"labels":      map[string]interface{}{LabelManagedBy: LabelManagedByVMHostRoutes},
			"annotations": map[string]interface{}{AnnotationBGPRouting: routing.Name},
		},
	}}
	noMatch := &meta.NoKindMatchError{GroupKind: VirtualMachineInstanceGVK.GroupKind(),
		SearchedVersions: []string{VirtualMachineInstanceGVK.Version}}
	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routing, config, testVMNamespace(), stale).
		WithStatusSubresource(routing, config).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if list.GetObjectKind().GroupVersionKind() == VirtualMachineInstanceGVK.GroupVersion().WithKind("VirtualMachineInstanceList") {
					return noMatch
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	r := &BGPRoutingReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: routing.Name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	updated := &networkingapi.BGPRouting{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(routing), updated); err != nil {
		t.Fatalf("get BGPRouting: %v", err)
	}
	if updated.Status.Phase != networkingapi.PhaseReady {
		t.Fatalf("phase = %s, want Ready", updated.Status.Phase)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, networkingapi.ConditionVMHostRoutesConfigured)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "VMIAPIUnavailable" {
		t.Fatalf("VM host-route condition = %#v, want False/VMIAPIUnavailable", condition)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), got); err != nil {
		t.Fatalf("existing route was removed: %v", err)
	}
}

func TestVMIRouteChangePredicate(t *testing.T) {
	base := testVMI("vms", "vm", "worker-a", "10.100.0.4")
	pred := vmiRouteChangePredicate()
	tests := []struct {
		name string
		edit func(*unstructured.Unstructured)
		want bool
	}{
		{"unrelated condition", func(vmi *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(vmi.Object, "Ready", "status", "conditions")
		}, false},
		{"address", func(vmi *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(vmi.Object, []interface{}{map[string]interface{}{"ipAddress": "10.100.0.5"}}, "status", "interfaces")
		}, true},
		{"node move", func(vmi *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(vmi.Object, "worker-b", "status", "nodeName")
		}, true},
		{"phase", func(vmi *unstructured.Unstructured) {
			_ = unstructured.SetNestedField(vmi.Object, "Succeeded", "status", "phase")
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updated := base.DeepCopy()
			tt.edit(updated)
			if got := pred.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: updated}); got != tt.want {
				t.Fatalf("predicate = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestRoutingReconcile_RepeatedReconcile_DoesNotRewriteSharedRouteAdvertisements(t *testing.T) {
	config := newReadyBGPCloudConfiguration()
	routingProd := newTestBGPRouting()
	routingStaging := &networkingapi.BGPRouting{
		ObjectMeta: metav1.ObjectMeta{Name: "staging"},
		Spec: networkingapi.BGPRoutingSpec{
			Network: networkingapi.NetworkConfig{Name: "staging", Subnets: []string{"10.200.0.0/16"}},
		},
	}
	nsProd := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "app1", Labels: map[string]string{LabelPrimaryUDN: "", LabelClusterUDN: "prod"}},
	}
	nsStaging := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "app2", Labels: map[string]string{LabelPrimaryUDN: "", LabelClusterUDN: "staging"}},
	}

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routingProd, routingStaging, config, nsProd, nsStaging).
		WithStatusSubresource(routingProd, routingStaging, config).
		Build()

	r := &BGPRoutingReconciler{Client: c, Scheme: s}

	prodReq := reconcile.Request{NamespacedName: types.NamespacedName{Name: "prod"}}
	stagingReq := reconcile.Request{NamespacedName: types.NamespacedName{Name: "staging"}}

	// Drive both CRs to Ready (finalizer-add + full pass each, matching
	// TestRoutingReconcile_FullReconcile's two-call convention). This
	// creates the single shared RouteAdvertisements object.
	_, _ = r.Reconcile(context.Background(), prodReq)
	if _, err := r.Reconcile(context.Background(), prodReq); err != nil {
		t.Fatalf("prod reconcile error: %v", err)
	}
	_, _ = r.Reconcile(context.Background(), stagingReq)
	if _, err := r.Reconcile(context.Background(), stagingReq); err != nil {
		t.Fatalf("staging reconcile error: %v", err)
	}

	ra := &unstructured.Unstructured{}
	ra.SetGroupVersionKind(RouteAdvertisementsGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Name: RouteAdvertisementName}, ra); err != nil {
		t.Fatalf("RouteAdvertisements not created: %v", err)
	}
	resourceVersionAfterFirstPass := ra.GetResourceVersion()

	// Reconcile both CRs again with no external state changed. This is the
	// scenario that used to loop forever: mapRAToRouting fans the shared
	// RA object's watch out to every BGPRouting CR, and each one used
	// to rewrite RA unconditionally, re-triggering all the others again.
	if _, err := r.Reconcile(context.Background(), prodReq); err != nil {
		t.Fatalf("prod repeat reconcile error: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), stagingReq); err != nil {
		t.Fatalf("staging repeat reconcile error: %v", err)
	}

	if err := c.Get(context.Background(), types.NamespacedName{Name: RouteAdvertisementName}, ra); err != nil {
		t.Fatalf("get RouteAdvertisements after repeat reconciles: %v", err)
	}
	if ra.GetResourceVersion() != resourceVersionAfterFirstPass {
		t.Fatalf("expected no rewrite of shared RouteAdvertisements across repeated reconciles of multiple CRs, resourceVersion changed from %q to %q",
			resourceVersionAfterFirstPass, ra.GetResourceVersion())
	}
}

func TestRoutingReconcile_NoNamespace(t *testing.T) {
	routing := newTestBGPRouting()
	routing.Finalizers = []string{RoutingFinalizerName}
	config := newReadyBGPCloudConfiguration()

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routing, config).
		WithStatusSubresource(routing, config).
		Build()

	r := &BGPRoutingReconciler{Client: c, Scheme: s}
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Errorf("expected 30s degraded requeue, got %v", result.RequeueAfter)
	}

	updated := &networkingapi.BGPRouting{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "prod"}, updated); err != nil {
		t.Fatalf("failed to get updated BGPRouting: %v", err)
	}
	if updated.Status.Phase != networkingapi.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
}

func TestRoutingReconcile_DeleteLastRemovesRA(t *testing.T) {
	now := metav1.Now()
	routing := newTestBGPRouting()
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

	r := &BGPRoutingReconciler{Client: c, Scheme: s}
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
	routing := newTestBGPRouting()
	routing.Finalizers = []string{RoutingFinalizerName}
	routing.DeletionTimestamp = &now

	other := &networkingapi.BGPRouting{
		ObjectMeta: metav1.ObjectMeta{Name: "staging"},
		Spec: networkingapi.BGPRoutingSpec{
			Network: networkingapi.NetworkConfig{
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

	r := &BGPRoutingReconciler{Client: c, Scheme: s}
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
	existing := &networkingapi.BGPRouting{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-prod"},
		Spec: networkingapi.BGPRoutingSpec{
			Network: networkingapi.NetworkConfig{
				Name: "prod", Subnets: []string{"10.100.0.0/16"},
			},
		},
	}
	duplicate := newTestBGPRouting()
	duplicate.Finalizers = []string{RoutingFinalizerName}
	config := newReadyBGPCloudConfiguration()
	namespace := testVMNamespace()
	stale := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "frrk8s.metallb.io/v1beta1", "kind": "FRRConfiguration",
		"metadata": map[string]interface{}{
			"name": "existing-route", "namespace": FRRNamespace,
			"labels":      map[string]interface{}{LabelManagedBy: LabelManagedByVMHostRoutes},
			"annotations": map[string]interface{}{AnnotationBGPRouting: duplicate.Name},
		},
	}}

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(existing, duplicate, config, namespace, stale).
		WithStatusSubresource(existing, duplicate, config).
		Build()

	r := &BGPRoutingReconciler{Client: c, Scheme: s}
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

	updated := &networkingapi.BGPRouting{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "prod"}, updated); err != nil {
		t.Fatalf("failed to get updated BGPRouting: %v", err)
	}
	if updated.Status.Phase != networkingapi.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
	routeCondition := meta.FindStatusCondition(updated.Status.Conditions, networkingapi.ConditionVMHostRoutesConfigured)
	if routeCondition == nil || routeCondition.Status != metav1.ConditionTrue ||
		routeCondition.Message != "Configured 0 VM host routes" {
		t.Errorf("VM host-route condition = %#v, want zero configured routes", routeCondition)
	}
	gotRoute := &unstructured.Unstructured{}
	gotRoute.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), gotRoute); !apierrors.IsNotFound(err) {
		t.Errorf("stale VM host route survived DuplicateNetwork pre-check: %v", err)
	}
}

// After the conflicting CR is deleted, the degraded duplicate recovers to Ready
// on the next reconcile (triggered in production by enqueueAllRoutings).
func TestRoutingReconcile_DuplicateNetwork_RecoversOnConflictDelete(t *testing.T) {
	existing := &networkingapi.BGPRouting{
		ObjectMeta: metav1.ObjectMeta{Name: "existing-prod"},
		Spec: networkingapi.BGPRoutingSpec{
			Network: networkingapi.NetworkConfig{
				Name: "prod", Subnets: []string{"10.100.0.0/16"},
			},
		},
	}
	duplicate := newTestBGPRouting()
	duplicate.Finalizers = []string{RoutingFinalizerName}
	config := newReadyBGPCloudConfiguration()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app1",
			Labels: map[string]string{
				LabelPrimaryUDN: "",
				LabelClusterUDN: "prod",
			},
		},
	}

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(existing, duplicate, config, ns).
		WithStatusSubresource(existing, duplicate, config).
		Build()

	r := &BGPRoutingReconciler{Client: c, Scheme: s}

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})
	if err != nil {
		t.Fatalf("first reconcile error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for terminal DuplicateNetwork, got %v", result.RequeueAfter)
	}
	before := &networkingapi.BGPRouting{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "prod"}, before); err != nil {
		t.Fatalf("failed to get BGPRouting before recovery: %v", err)
	}
	if before.Status.Phase != networkingapi.PhaseDegraded {
		t.Fatalf("expected Degraded after duplicate, got %s", before.Status.Phase)
	}

	if err := c.Delete(context.Background(), existing); err != nil {
		t.Fatalf("failed to delete existing-prod: %v", err)
	}

	result2, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})
	if err != nil {
		t.Fatalf("recovery reconcile error: %v", err)
	}
	if result2.RequeueAfter != 5*time.Minute {
		t.Errorf("expected 5m resync after recovery, got %v", result2.RequeueAfter)
	}

	recovered := &networkingapi.BGPRouting{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "prod"}, recovered); err != nil {
		t.Fatalf("failed to get recovered BGPRouting: %v", err)
	}
	if recovered.Status.Phase != networkingapi.PhaseReady {
		t.Errorf("expected Ready after conflict resolution, got %s", recovered.Status.Phase)
	}
}

// NamespaceNotReady stays transient (regression guard — must still be 30s).
func TestRoutingReconcile_NamespaceNotReady_StillTransient(t *testing.T) {
	routing := newTestBGPRouting()
	routing.Finalizers = []string{RoutingFinalizerName}
	config := newReadyBGPCloudConfiguration()

	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(routing, config).
		WithStatusSubresource(routing, config).
		Build()

	r := &BGPRoutingReconciler{Client: c, Scheme: s}
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
	routing1 := newTestBGPRouting()
	routing2 := &networkingapi.BGPRouting{
		ObjectMeta: metav1.ObjectMeta{Name: "staging"},
		Spec: networkingapi.BGPRoutingSpec{
			Network: networkingapi.NetworkConfig{Name: "staging", Subnets: []string{"10.200.0.0/16"}},
		},
	}
	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(routing1, routing2).Build()
	r := &BGPRoutingReconciler{Client: c, Scheme: s}

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

func TestMapClusterUDNToRouting_Managed(t *testing.T) {
	routing := newTestBGPRouting()
	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(routing).Build()
	r := &BGPRoutingReconciler{Client: c, Scheme: s}

	cudn := &unstructured.Unstructured{}
	cudn.SetName(ClusterUDNNamePrefix + "prod")
	cudn.SetLabels(map[string]string{LabelManagedBy: LabelManagedByVal})

	requests := r.mapClusterUDNToRouting(context.Background(), cudn)
	if len(requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(requests))
	}
	if requests[0].Name != "prod" {
		t.Errorf("expected request for 'prod', got %q", requests[0].Name)
	}
}

func TestMapClusterUDNToRouting_Unmanaged(t *testing.T) {
	routing := newTestBGPRouting()
	s := routingTestScheme()
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(routing).Build()
	r := &BGPRoutingReconciler{Client: c, Scheme: s}

	cudn := &unstructured.Unstructured{}
	cudn.SetName(ClusterUDNNamePrefix + "prod")
	cudn.SetLabels(map[string]string{"other": "label"})

	requests := r.mapClusterUDNToRouting(context.Background(), cudn)
	if len(requests) != 0 {
		t.Errorf("expected 0 requests for unmanaged ClusterUDN, got %d", len(requests))
	}
}

// TestRoutingReconcile_CUDNSpecInvalid_NoRequeue: an invalid ClusterUDN spec (e.g. bad CIDR)
// is terminal — the user must correct spec.network in the BGPRouting.
func TestRoutingReconcile_CUDNSpecInvalid_NoRequeue(t *testing.T) {
	routing := newTestBGPRouting()
	routing.Finalizers = []string{RoutingFinalizerName}
	config := newReadyBGPCloudConfiguration()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app1",
			Labels: map[string]string{
				LabelPrimaryUDN: "",
				LabelClusterUDN: "prod",
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
				if obj.GetName() == ClusterUDNNamePrefix+"prod" {
					return invalidErr
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()

	r := &BGPRoutingReconciler{Client: c, Scheme: s}
	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "prod"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for terminal CUDNSpecInvalid, got %v", result.RequeueAfter)
	}

	updated := &networkingapi.BGPRouting{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "prod"}, updated); err != nil {
		t.Fatalf("failed to get updated BGPRouting: %v", err)
	}
	if updated.Status.Phase != networkingapi.PhaseDegraded {
		t.Errorf("expected Degraded, got %s", updated.Status.Phase)
	}
	for _, cond := range updated.Status.Conditions {
		if cond.Type == networkingapi.ConditionNetworkCreated {
			if cond.Reason != ReasonCUDNSpecInvalid {
				t.Errorf("expected reason CUDNSpecInvalid, got %s", cond.Reason)
			}
			return
		}
	}
	t.Error("NetworkCreated condition with CUDNSpecInvalid reason not found")
}

func nodeWithLabels(labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a", Labels: labels}}
}

// Node labels decide whether a node is in the router pool and which peer group
// it belongs to, so a label change alters what EnsureVMHostRoutes writes.
// Nothing else about a node does, and node status churns constantly.
func TestNodeLabelChangePredicate(t *testing.T) {
	p := nodeLabelChangePredicate()

	same := nodeWithLabels(map[string]string{"bgp_router": "true"})
	churned := same.DeepCopy()
	churned.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.1"}}
	churned.ResourceVersion = "2"
	if p.Update(event.UpdateEvent{ObjectOld: same, ObjectNew: churned}) {
		t.Error("a node status change with unchanged labels triggered a reconcile")
	}

	relabelled := same.DeepCopy()
	delete(relabelled.Labels, "bgp_router")
	if !p.Update(event.UpdateEvent{ObjectOld: same, ObjectNew: relabelled}) {
		t.Error("a node leaving the router pool did not trigger a reconcile")
	}

	if !p.Delete(event.DeleteEvent{Object: same}) {
		t.Error("a node being deleted did not trigger a reconcile")
	}
}

// EnsureVMHostRoutes reads spec.routerNodeSelector, the BGP settings and, on a
// cloud, status.peerGroups. None of those reaching this controller means the VM
// host routes keep the old neighbour set until the next resync, while bgp-cc-N
// is rewritten at once by the controller that does watch them.
func TestConfigRelevantToRoutingPredicate(t *testing.T) {
	p := configRelevantToRoutingPredicate()

	base := newReadyBGPCloudConfiguration()
	base.Generation = 1

	conditionsOnly := base.DeepCopy()
	conditionsOnly.Status.Conditions = []metav1.Condition{{
		Type: ConditionDeletionBlocked, Status: metav1.ConditionFalse, Reason: ReasonReconciled,
		LastTransitionTime: metav1.Now(),
	}}
	if p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: conditionsOnly}) {
		t.Error("a condition-only status change triggered a reconcile of every BGPRouting")
	}

	specChanged := base.DeepCopy()
	specChanged.Generation = 2
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: specChanged}) {
		t.Error("a spec change did not trigger a reconcile")
	}

	phaseChanged := base.DeepCopy()
	phaseChanged.Status.Phase = networkingapi.PhaseDegraded
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: phaseChanged}) {
		t.Error("a phase change did not trigger a reconcile")
	}

	peersChanged := base.DeepCopy()
	peersChanged.Status.PeerGroups = []networkingapi.PeerGroupStatus{{Key: "a"}}
	if !p.Update(event.UpdateEvent{ObjectOld: base, ObjectNew: peersChanged}) {
		t.Error("a discovered peer group change did not trigger a reconcile")
	}
}
