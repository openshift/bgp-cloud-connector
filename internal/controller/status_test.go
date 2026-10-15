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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

// assertSummary checks the Available/Progressing/Degraded aggregate conditions.
// It is shared by the two controller test files: every terminal reconcile
// outcome sets exactly one of the three True, and the assertion pins which.
func assertSummary(t *testing.T, conds []metav1.Condition, wantAvail, wantProg, wantDegraded metav1.ConditionStatus) {
	t.Helper()
	for _, tc := range []struct {
		condType string
		want     metav1.ConditionStatus
	}{
		{ConditionAvailable, wantAvail},
		{ConditionProgressing, wantProg},
		{ConditionDegraded, wantDegraded},
	} {
		got := meta.FindStatusCondition(conds, tc.condType)
		if got == nil {
			t.Errorf("summary condition %q not set", tc.condType)
			continue
		}
		if got.Status != tc.want {
			t.Errorf("summary condition %q = %s, want %s", tc.condType, got.Status, tc.want)
		}
		if got.Reason == "" {
			t.Errorf("summary condition %q has empty reason", tc.condType)
		}
	}
}

func TestSetSummaryConditions(t *testing.T) {
	cases := []struct {
		name                             string
		target                           networkingapi.PhaseType
		wantAvail, wantProg, wantDegrade metav1.ConditionStatus
	}{
		{"ready", networkingapi.PhaseReady, metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionFalse},
		{"configuring", networkingapi.PhaseConfiguring, metav1.ConditionFalse, metav1.ConditionTrue, metav1.ConditionFalse},
		{"pending", networkingapi.PhasePending, metav1.ConditionFalse, metav1.ConditionTrue, metav1.ConditionFalse},
		{"degraded", networkingapi.PhaseDegraded, metav1.ConditionFalse, metav1.ConditionFalse, metav1.ConditionTrue},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var phase networkingapi.PhaseType
			var conds []metav1.Condition
			setSummaryConditions(&phase, &conds, 7, tc.target, ReasonAsExpected, "message")

			if phase != tc.target {
				t.Errorf("phase = %s, want %s", phase, tc.target)
			}
			if len(conds) != 3 {
				t.Fatalf("expected 3 summary conditions, got %d", len(conds))
			}
			assertSummary(t, conds, tc.wantAvail, tc.wantProg, tc.wantDegrade)
			for _, c := range conds {
				if c.ObservedGeneration != 7 {
					t.Errorf("condition %q observedGeneration = %d, want 7", c.Type, c.ObservedGeneration)
				}
			}
		})
	}
}

func TestSetSummaryConditions_StableAcrossCalls(t *testing.T) {
	// A settled reconcile must produce byte-identical summary conditions, or the
	// baseline diff would rewrite status every pass and hot-loop the controller.
	var phase networkingapi.PhaseType
	var conds []metav1.Condition
	setSummaryConditions(&phase, &conds, 1, networkingapi.PhaseReady, ReasonAsExpected, "ready")
	first := meta.FindStatusCondition(conds, ConditionAvailable).LastTransitionTime

	setSummaryConditions(&phase, &conds, 1, networkingapi.PhaseReady, ReasonAsExpected, "ready")
	second := meta.FindStatusCondition(conds, ConditionAvailable).LastTransitionTime

	if !first.Equal(&second) {
		t.Errorf("LastTransitionTime changed on an unchanged condition: %v -> %v", first, second)
	}
}

func TestConfigStatusEqual(t *testing.T) {
	base := networkingapi.BGPCloudConfigurationStatus{
		Phase:              networkingapi.PhaseConfiguring,
		ObservedGeneration: 1,
		Conditions: []metav1.Condition{
			{Type: networkingapi.ConditionFRRNamespaceReady, Status: metav1.ConditionFalse, Reason: ReasonWaitingForFRR},
		},
	}
	same := base.DeepCopy()
	if !configStatusEqual(base, *same) {
		t.Fatal("expected DeepCopy status to be equal")
	}
	diff := base.DeepCopy()
	diff.Phase = networkingapi.PhaseReady
	if configStatusEqual(base, *diff) {
		t.Fatal("expected different phase to be unequal")
	}
}

func TestRoutingStatusEqual_NilVsEmptyConditions(t *testing.T) {
	withNil := networkingapi.BGPRoutingStatus{
		Phase:              networkingapi.PhasePending,
		ObservedGeneration: 1,
	}
	withEmpty := networkingapi.BGPRoutingStatus{
		Phase:              networkingapi.PhasePending,
		ObservedGeneration: 1,
		Conditions:         []metav1.Condition{},
	}
	if !routingStatusEqual(withNil, withEmpty) {
		t.Fatal("expected nil and empty conditions to compare equal with Semantic equality")
	}
}

func TestPatchConfigStatus_SkipsUnchangedStatus(t *testing.T) {
	ctx := context.Background()

	config := &networkingapi.BGPCloudConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       SingletonName,
			Generation: 1,
		},
		Status: networkingapi.BGPCloudConfigurationStatus{
			Phase:              networkingapi.PhaseConfiguring,
			ObservedGeneration: 1,
		},
	}
	baseline := config.Status.DeepCopy()

	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(config).WithObjects(config).Build()
	r := &BGPCloudConfigurationReconciler{Client: c, Scheme: testScheme()}

	before := config.DeepCopy()
	if err := r.patchConfigStatus(ctx, config, *baseline, func(c *networkingapi.BGPCloudConfiguration) {
		c.Status.Phase = networkingapi.PhaseConfiguring
		c.Status.ObservedGeneration = c.Generation
	}); err != nil {
		t.Fatalf("patchConfigStatus: %v", err)
	}

	after := &networkingapi.BGPCloudConfiguration{}
	if err := c.Get(ctx, types.NamespacedName{Name: SingletonName}, after); err != nil {
		t.Fatalf("get config: %v", err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Fatalf("expected no status update, resourceVersion changed from %q to %q", before.ResourceVersion, after.ResourceVersion)
	}
}

func TestPatchConfigStatus_WritesWhenPhaseChanges(t *testing.T) {
	ctx := context.Background()

	config := &networkingapi.BGPCloudConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       SingletonName,
			Generation: 1,
		},
		Status: networkingapi.BGPCloudConfigurationStatus{
			Phase:              networkingapi.PhaseConfiguring,
			ObservedGeneration: 1,
		},
	}
	baseline := config.Status.DeepCopy()

	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(config).WithObjects(config).Build()
	r := &BGPCloudConfigurationReconciler{Client: c, Scheme: testScheme()}

	if err := r.patchConfigStatus(ctx, config, *baseline, func(c *networkingapi.BGPCloudConfiguration) {
		c.Status.Phase = networkingapi.PhaseReady
		c.Status.ObservedGeneration = c.Generation
	}); err != nil {
		t.Fatalf("patchConfigStatus: %v", err)
	}

	after := &networkingapi.BGPCloudConfiguration{}
	if err := c.Get(ctx, types.NamespacedName{Name: SingletonName}, after); err != nil {
		t.Fatalf("get config: %v", err)
	}
	if after.Status.Phase != networkingapi.PhaseReady {
		t.Fatalf("expected phase Ready, got %q", after.Status.Phase)
	}
}

func TestPatchRoutingStatus_SkipsUnchangedPending(t *testing.T) {
	ctx := context.Background()

	routing := &networkingapi.BGPRouting{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "prod",
			Generation: 1,
		},
		Status: networkingapi.BGPRoutingStatus{
			Phase:              networkingapi.PhasePending,
			ObservedGeneration: 1,
		},
	}
	baseline := routing.Status.DeepCopy()

	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(routing).WithObjects(routing).Build()
	r := &BGPRoutingReconciler{Client: c, Scheme: testScheme()}

	before := routing.DeepCopy()
	if err := r.patchRoutingStatus(ctx, routing, *baseline, func(rt *networkingapi.BGPRouting) {
		rt.Status.Phase = networkingapi.PhasePending
		rt.Status.Conditions = nil
	}); err != nil {
		t.Fatalf("patchRoutingStatus: %v", err)
	}

	after := &networkingapi.BGPRouting{}
	if err := c.Get(ctx, types.NamespacedName{Name: "prod"}, after); err != nil {
		t.Fatalf("get routing: %v", err)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Fatalf("expected no status update, resourceVersion changed from %q to %q", before.ResourceVersion, after.ResourceVersion)
	}
}

func TestPersistNetworkOwnership_RejectsStaleResourceVersion(t *testing.T) {
	ctx := context.Background()

	config := &networkingapi.BGPCloudConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name: SingletonName,
		},
	}

	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(config).WithObjects(config).Build()
	r := &BGPCloudConfigurationReconciler{Client: c, Scheme: testScheme()}

	stale := &networkingapi.BGPCloudConfiguration{}
	if err := c.Get(ctx, types.NamespacedName{Name: SingletonName}, stale); err != nil {
		t.Fatalf("get stale config: %v", err)
	}

	live := stale.DeepCopy()
	live.Status.FRRProviderOwnership = networkingapi.NetworkPatchOwnershipOwned
	if err := c.Status().Update(ctx, live); err != nil {
		t.Fatalf("seed live ownership: %v", err)
	}

	stale.Status.RouteAdvertisementsOwnership = networkingapi.NetworkPatchOwnershipExternal
	err := r.persistNetworkOwnership(ctx, stale)
	if !apierrors.IsConflict(err) {
		t.Fatalf("expected conflict from stale resourceVersion, got %v", err)
	}
}
