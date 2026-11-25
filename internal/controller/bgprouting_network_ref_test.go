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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

func newReferenceRouting(name string) *networkingapi.BGPRouting {
	routing := newTestBGPRouting()
	routing.Name = name
	routing.UID = types.UID(name + "-uid")
	routing.Spec.Network = nil
	routing.Spec.NetworkRef = &networkingapi.RoutingNetworkReference{Kind: ClusterUDNGVK.Kind, Name: "vm-net"}
	return routing
}

func newReferenceCUDN(topology string) *unstructured.Unstructured {
	cudn := &unstructured.Unstructured{}
	cudn.SetGroupVersionKind(ClusterUDNGVK)
	cudn.SetName("vm-net")
	cudn.SetUID("cudn-uid")
	cudn.SetLabels(map[string]string{labelRoutingNetworkRef: string(cudn.GetUID())})
	network := map[string]interface{}{
		"topology":                topology,
		strings.ToLower(topology): map[string]interface{}{"role": primaryNetworkRole},
	}
	cudn.Object["spec"] = map[string]interface{}{"network": network}
	return cudn
}

func runReferenceReconcile(t *testing.T, r *BGPRoutingReconciler, routing *networkingapi.BGPRouting) {
	t.Helper()
	if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(routing)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(routing), routing); err != nil {
		t.Fatal(err)
	}
}

func getReferenceAdvertisement(t *testing.T, c client.Client, routing *networkingapi.BGPRouting) *unstructured.Unstructured {
	t.Helper()
	ra := &unstructured.Unstructured{}
	ra.SetGroupVersionKind(RouteAdvertisementsGVK)
	if err := c.Get(t.Context(), client.ObjectKey{Name: referencedAdvertisementName(routing.Spec.NetworkRef.Name)}, ra); err != nil {
		t.Fatal(err)
	}
	return ra
}

func assertReferenceAdvertisementMissing(t *testing.T, c client.Client, routing *networkingapi.BGPRouting) {
	t.Helper()
	ra := &unstructured.Unstructured{}
	ra.SetGroupVersionKind(RouteAdvertisementsGVK)
	if err := c.Get(t.Context(), client.ObjectKey{Name: referencedAdvertisementName(routing.Spec.NetworkRef.Name)}, ra); !apierrors.IsNotFound(err) {
		t.Fatalf("expected dedicated advertisement to be absent, got %v", err)
	}
}

func requireReferenceCondition(t *testing.T, routing *networkingapi.BGPRouting, kind string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := meta.FindStatusCondition(routing.Status.Conditions, kind)
	if condition == nil || condition.Status != status || condition.Reason != reason {
		t.Fatalf("expected %s=%s (%s), got %+v", kind, status, reason, condition)
	}
}

func setAdvertisementConditions(t *testing.T, c client.Client, ra *unstructured.Unstructured, conditions ...map[string]interface{}) {
	t.Helper()
	items := make([]interface{}, len(conditions))
	for i, condition := range conditions {
		items[i] = condition
	}
	if err := unstructured.SetNestedSlice(ra.Object, items, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if err := c.Status().Update(t.Context(), ra); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkRefReconcileStatusAndCleanup(t *testing.T) {
	for _, topology := range []string{layer2Topology, layer3Topology} {
		t.Run(topology, func(t *testing.T) {
			routing := newReferenceRouting("referenced")
			cudn := newReferenceCUDN(topology)
			raStatus := &unstructured.Unstructured{}
			raStatus.SetGroupVersionKind(RouteAdvertisementsGVK)
			c := fake.NewClientBuilder().WithScheme(routingTestScheme()).
				WithObjects(routing, cudn, newReadyBGPCloudConfiguration()).
				WithStatusSubresource(routing, raStatus).Build()
			r := &BGPRoutingReconciler{Client: c}

			runReferenceReconcile(t, r, routing)
			ra := getReferenceAdvertisement(t, c, routing)
			if !referencedAdvertisementOwnedBy(ra, routing) {
				t.Fatalf("unexpected advertisement ownership: %+v", ra.GetOwnerReferences())
			}
			if routing.Status.Phase != networkingapi.PhaseConfiguring || routing.Status.RouteAdvertisementsRef == nil ||
				routing.Status.RouteAdvertisementsRef.Name != ra.GetName() || routing.Status.NetworkRef == nil ||
				routing.Status.NetworkRef.UID != string(cudn.GetUID()) || routing.Status.NetworkRef.Topology != topology ||
				routing.Status.NetworkRef.Role != primaryNetworkRole {
				t.Fatalf("unexpected pending status: %+v", routing.Status)
			}
			requireReferenceCondition(t, routing, networkingapi.ConditionNetworkResolved, metav1.ConditionTrue, "Resolved")
			assertSummary(t, routing.Status.Conditions, metav1.ConditionFalse, metav1.ConditionTrue, metav1.ConditionFalse)
			requireReferenceCondition(t, routing, networkingapi.ConditionRouteAdvertisementsAccepted, metav1.ConditionUnknown, "WaitingForOVN")
			selectors, _, _ := unstructured.NestedSlice(ra.Object, "spec", "networkSelectors")
			wantSelectors := []interface{}{map[string]interface{}{
				"networkSelectionType": "ClusterUserDefinedNetworks",
				"clusterUserDefinedNetworkSelector": map[string]interface{}{
					"networkSelector": map[string]interface{}{
						"matchLabels": map[string]interface{}{labelRoutingNetworkRef: string(cudn.GetUID())},
					},
				},
			}}
			if !reflect.DeepEqual(selectors, wantSelectors) {
				t.Fatalf("unexpected selectors: %+v", selectors)
			}
			selector, _, _ := unstructured.NestedStringMap(ra.Object, "spec", "frrConfigurationSelector", "matchLabels")
			if !reflect.DeepEqual(selector, map[string]string{LabelManagedBy: LabelManagedByVal}) {
				t.Fatalf("unexpected peering selector: %+v", selector)
			}

			accepted := map[string]interface{}{
				"type": "Accepted", "status": "True", "reason": "Accepted", "message": "Test acceptance",
				"observedGeneration": ra.GetGeneration() + 1,
			}
			setAdvertisementConditions(t, c, ra, accepted)
			runReferenceReconcile(t, r, routing)
			requireReferenceCondition(t, routing, networkingapi.ConditionRouteAdvertisementsAccepted, metav1.ConditionUnknown, "WaitingForOVN")

			accepted["observedGeneration"] = ra.GetGeneration()
			accepted["status"], accepted["reason"] = "False", "ConfigurationError"
			setAdvertisementConditions(t, c, ra, accepted)
			runReferenceReconcile(t, r, routing)
			requireReferenceCondition(t, routing, networkingapi.ConditionRouteAdvertisementsAccepted, metav1.ConditionFalse, "ConfigurationError")
			if routing.Status.Phase != networkingapi.PhaseDegraded {
				t.Fatalf("expected Degraded, got %s", routing.Status.Phase)
			}

			accepted["status"], accepted["reason"] = "True", "Accepted"
			setAdvertisementConditions(t, c, ra, accepted, map[string]interface{}{
				"type": "Degraded", "status": "True", "reason": "NodeFailed", "message": "Test node failure",
				"observedGeneration": ra.GetGeneration(),
			})
			runReferenceReconcile(t, r, routing)
			requireReferenceCondition(t, routing, "RouteAdvertisementsDegraded", metav1.ConditionTrue, "NodeFailed")
			if routing.Status.Phase != networkingapi.PhaseDegraded {
				t.Fatalf("expected Degraded, got %s", routing.Status.Phase)
			}

			setAdvertisementConditions(t, c, ra, accepted)
			runReferenceReconcile(t, r, routing)
			if routing.Status.Phase != networkingapi.PhaseReady || meta.FindStatusCondition(routing.Status.Conditions, "RouteAdvertisementsDegraded") != nil {
				t.Fatalf("expected Ready after OVN recovered, got %+v", routing.Status)
			}
			assertSummary(t, routing.Status.Conditions, metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionFalse)
			version := getReferenceAdvertisement(t, c, routing).GetResourceVersion()
			statusVersion := routing.ResourceVersion
			runReferenceReconcile(t, r, routing)
			if getReferenceAdvertisement(t, c, routing).GetResourceVersion() != version || routing.ResourceVersion != statusVersion {
				t.Fatal("steady-state reconcile rewrote the advertisement or status")
			}

			if err := EnsureRouteAdvertisements(t.Context(), c); err != nil {
				t.Fatal(err)
			}
			if err := c.Delete(t.Context(), routing); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(routing)}); err != nil {
				t.Fatal(err)
			}
			assertReferenceAdvertisementMissing(t, c, routing)
			original := cudn.DeepCopy()
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(cudn), cudn); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cudn.Object["spec"], original.Object["spec"]) || !reflect.DeepEqual(cudn.GetLabels(), original.GetLabels()) {
				t.Fatalf("reference mode changed the existing CUDN: %+v", cudn.Object)
			}
			shared := &unstructured.Unstructured{}
			shared.SetGroupVersionKind(RouteAdvertisementsGVK)
			if err := c.Get(t.Context(), client.ObjectKey{Name: RouteAdvertisementName}, shared); err != nil {
				t.Fatalf("reference cleanup removed the shared advertisement: %v", err)
			}
		})
	}
}

func TestNetworkRefRejectsInvalidTargetsAndWithdraws(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		change func(context.Context, client.Client, *unstructured.Unstructured) error
	}{
		{name: "deleted", reason: "NetworkNotFound", change: func(ctx context.Context, c client.Client, cudn *unstructured.Unstructured) error {
			return c.Delete(ctx, cudn)
		}},
		{name: "secondary", reason: "UnsupportedNetwork", change: func(ctx context.Context, c client.Client, cudn *unstructured.Unstructured) error {
			if err := unstructured.SetNestedField(cudn.Object, "Secondary", "spec", "network", "layer2", "role"); err != nil {
				return err
			}
			return c.Update(ctx, cudn)
		}},
		{name: "missing-opt-in", reason: "OptInRequired", change: func(ctx context.Context, c client.Client, cudn *unstructured.Unstructured) error {
			cudn.SetLabels(nil)
			return c.Update(ctx, cudn)
		}},
		{name: "copied-opt-in", reason: "ConflictingSelector", change: func(ctx context.Context, c client.Client, cudn *unstructured.Unstructured) error {
			other := newReferenceCUDN(layer2Topology)
			other.SetName("other")
			other.SetUID("other-uid")
			return c.Create(ctx, other)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			routing := newReferenceRouting("referenced")
			cudn := newReferenceCUDN(layer2Topology)
			c := fake.NewClientBuilder().WithScheme(routingTestScheme()).
				WithObjects(routing, cudn, newReadyBGPCloudConfiguration()).WithStatusSubresource(routing).Build()
			r := &BGPRoutingReconciler{Client: c}
			runReferenceReconcile(t, r, routing)
			getReferenceAdvertisement(t, c, routing)
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(cudn), cudn); err != nil {
				t.Fatal(err)
			}
			if err := tc.change(t.Context(), c, cudn); err != nil {
				t.Fatal(err)
			}

			runReferenceReconcile(t, r, routing)
			requireReferenceCondition(t, routing, networkingapi.ConditionNetworkResolved, metav1.ConditionFalse, tc.reason)
			if routing.Status.RouteAdvertisementsRef != nil {
				t.Fatalf("unexpected advertisement reference: %+v", routing.Status.RouteAdvertisementsRef)
			}
			assertReferenceAdvertisementMissing(t, c, routing)
		})
	}
}

func TestNetworkRefDoesNotTakeOverAnotherAdvertisement(t *testing.T) {
	first := newReferenceRouting("first")
	second := newReferenceRouting("second")
	c := fake.NewClientBuilder().WithScheme(routingTestScheme()).
		WithObjects(first, second, newReferenceCUDN(layer2Topology), newReadyBGPCloudConfiguration()).
		WithStatusSubresource(first).Build()
	r := &BGPRoutingReconciler{Client: c}
	runReferenceReconcile(t, r, first)
	runReferenceReconcile(t, r, second)
	requireReferenceCondition(t, second, networkingapi.ConditionRouteAdvertisementsAccepted, metav1.ConditionUnknown, ReasonRAFailed)
	if err := c.Delete(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(second)}); err != nil {
		t.Fatal(err)
	}
	if ra := getReferenceAdvertisement(t, c, first); !referencedAdvertisementOwnedBy(ra, first) {
		t.Fatalf("the first advertisement changed ownership: %+v", ra.GetOwnerReferences())
	}
}

func TestNetworkRefCoexistsWithLegacyProvisioning(t *testing.T) {
	legacy := newTestBGPRouting()
	legacy.Finalizers = []string{RoutingFinalizerName}
	routing := newReferenceRouting("referenced")
	cudn := newReferenceCUDN(layer2Topology)
	c := fake.NewClientBuilder().WithScheme(routingTestScheme()).
		WithObjects(legacy, routing, cudn, newReadyBGPCloudConfiguration()).
		WithStatusSubresource(routing).Build()
	r := &BGPRoutingReconciler{Client: c}

	requests := r.mapClusterUDNToRouting(t.Context(), cudn)
	if len(requests) != 1 || requests[0].Name != routing.Name {
		t.Fatalf("CUDN change should enqueue its reference, got %v", requests)
	}
	runReferenceReconcile(t, r, routing)

	if err := EnsureRouteAdvertisements(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(legacy)}); err != nil {
		t.Fatal(err)
	}
	shared := &unstructured.Unstructured{}
	shared.SetGroupVersionKind(RouteAdvertisementsGVK)
	if err := c.Get(t.Context(), client.ObjectKey{Name: RouteAdvertisementName}, shared); !apierrors.IsNotFound(err) {
		t.Fatalf("last provisioning request should remove the shared advertisement, got %v", err)
	}
	getReferenceAdvertisement(t, c, routing)
}

func TestProvisioningPreservesOptedInReferenceNetwork(t *testing.T) {
	routing := newTestBGPRouting()
	cudn := newReferenceCUDN(layer2Topology)
	cudn.SetName(ClusterUDNNamePrefix + routing.Spec.Network.Name)
	c := fake.NewClientBuilder().WithScheme(routingTestScheme()).WithObjects(cudn).Build()

	if _, err := EnsureClusterUDN(t.Context(), c, routing); err == nil {
		t.Fatal("provisioning took over an existing network reserved for reference mode")
	}
	if err := DeleteClusterUDN(t.Context(), c, routing.Spec.Network.Name); err != nil {
		t.Fatal(err)
	}
	got := cudn.DeepCopy()
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(cudn), got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Object["spec"], cudn.Object["spec"]) || !reflect.DeepEqual(got.GetLabels(), cudn.GetLabels()) {
		t.Fatalf("provisioning modified the opted-in CUDN: %+v", got.Object)
	}
}

func TestNetworkRefWaitsForPeering(t *testing.T) {
	routing := newReferenceRouting("referenced")
	cudn := newReferenceCUDN(layer2Topology)
	c := fake.NewClientBuilder().WithScheme(routingTestScheme()).
		WithObjects(routing, cudn).WithStatusSubresource(routing).Build()
	r := &BGPRoutingReconciler{Client: c}

	runReferenceReconcile(t, r, routing)
	requireReferenceCondition(t, routing, networkingapi.ConditionRouteAdvertisementsAccepted, metav1.ConditionUnknown, "WaitingForPeering")
	assertReferenceAdvertisementMissing(t, c, routing)
	if err := c.Create(t.Context(), newReadyBGPCloudConfiguration()); err != nil {
		t.Fatal(err)
	}
	runReferenceReconcile(t, r, routing)
	requireReferenceCondition(t, routing, networkingapi.ConditionRouteAdvertisementsAccepted, metav1.ConditionUnknown, "WaitingForOVN")
	getReferenceAdvertisement(t, c, routing)
}
