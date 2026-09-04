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

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFRRConfigObject(name string, labels map[string]interface{}, nodeSelectorValue string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "frrk8s.metallb.io/v1beta1",
			"kind":       "FRRConfiguration",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": FRRNamespace,
				"labels":    labels,
			},
			"spec": map[string]interface{}{
				"nodeSelector": map[string]interface{}{
					"matchLabels": map[string]interface{}{"bgp_router": nodeSelectorValue},
				},
			},
		},
	}
}

func TestCreateOrUpdate_CreatesWhenMissing(t *testing.T) {
	ctx := context.Background()
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	c := fake.NewClientBuilder().WithScheme(s).Build()

	desired := newFRRConfigObject("bgp-cc-1", map[string]interface{}{LabelManagedBy: LabelManagedByVal}, "true")
	if err := createOrUpdate(ctx, c, desired); err != nil {
		t.Fatalf("createOrUpdate: %v", err)
	}

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-1", Namespace: FRRNamespace}, got); err != nil {
		t.Fatalf("expected object to be created: %v", err)
	}
}

func TestCreateOrUpdate_SkipsUpdateWhenSpecAndLabelsUnchanged(t *testing.T) {
	ctx := context.Background()
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	existing := newFRRConfigObject("bgp-cc-1", map[string]interface{}{LabelManagedBy: LabelManagedByVal}, "true")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()

	before := &unstructured.Unstructured{}
	before.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-1", Namespace: FRRNamespace}, before); err != nil {
		t.Fatalf("get before: %v", err)
	}

	desired := newFRRConfigObject("bgp-cc-1", map[string]interface{}{LabelManagedBy: LabelManagedByVal}, "true")
	if err := createOrUpdate(ctx, c, desired); err != nil {
		t.Fatalf("createOrUpdate: %v", err)
	}

	after := &unstructured.Unstructured{}
	after.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-1", Namespace: FRRNamespace}, after); err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.GetResourceVersion() != before.GetResourceVersion() {
		t.Fatalf("expected no update when spec and labels already match, resourceVersion changed from %q to %q",
			before.GetResourceVersion(), after.GetResourceVersion())
	}
}

func TestCreateOrUpdate_UpdatesWhenSpecChanges(t *testing.T) {
	ctx := context.Background()
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	existing := newFRRConfigObject("bgp-cc-1", map[string]interface{}{LabelManagedBy: LabelManagedByVal}, "true")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()

	before := &unstructured.Unstructured{}
	before.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-1", Namespace: FRRNamespace}, before); err != nil {
		t.Fatalf("get before: %v", err)
	}

	desired := newFRRConfigObject("bgp-cc-1", map[string]interface{}{LabelManagedBy: LabelManagedByVal}, "false")
	if err := createOrUpdate(ctx, c, desired); err != nil {
		t.Fatalf("createOrUpdate: %v", err)
	}

	after := &unstructured.Unstructured{}
	after.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-1", Namespace: FRRNamespace}, after); err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.GetResourceVersion() == before.GetResourceVersion() {
		t.Fatal("expected an update when spec changes, resourceVersion did not change")
	}
	value, _, _ := unstructured.NestedString(after.Object, "spec", "nodeSelector", "matchLabels", "bgp_router")
	if value != "false" {
		t.Fatalf("expected updated spec to be persisted, got bgp_router=%q", value)
	}
}

func TestCreateOrUpdate_UpdatesWhenManagedLabelMissing(t *testing.T) {
	ctx := context.Background()
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	// Same spec as desired, but missing the managed-by label.
	existing := newFRRConfigObject("bgp-cc-1", map[string]interface{}{}, "true")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()

	before := &unstructured.Unstructured{}
	before.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-1", Namespace: FRRNamespace}, before); err != nil {
		t.Fatalf("get before: %v", err)
	}

	desired := newFRRConfigObject("bgp-cc-1", map[string]interface{}{LabelManagedBy: LabelManagedByVal}, "true")
	if err := createOrUpdate(ctx, c, desired); err != nil {
		t.Fatalf("createOrUpdate: %v", err)
	}

	after := &unstructured.Unstructured{}
	after.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-1", Namespace: FRRNamespace}, after); err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.GetResourceVersion() == before.GetResourceVersion() {
		t.Fatal("expected an update when the managed-by label is missing, resourceVersion did not change")
	}
	if after.GetLabels()[LabelManagedBy] != LabelManagedByVal {
		t.Fatalf("expected managed-by label to be set, got %q", after.GetLabels()[LabelManagedBy])
	}
}

func TestSpecEqual(t *testing.T) {
	a := newFRRConfigObject("x", nil, "true")
	b := newFRRConfigObject("x", nil, "true")
	if !specEqual(a, b) {
		t.Error("expected identical specs to be equal")
	}

	c := newFRRConfigObject("x", nil, "false")
	if specEqual(a, c) {
		t.Error("expected different specs to be unequal")
	}
}

func TestSpecEqual_MissingSpecKey(t *testing.T) {
	a := &unstructured.Unstructured{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "x"}}}
	b := &unstructured.Unstructured{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "x"}}}
	if !specEqual(a, b) {
		t.Error("expected two objects with no spec key to be equal")
	}
}

// TestSpecEqual_IgnoresFieldsOnlyExistingSets pins the reason specEqual can't
// use a plain DeepEqual: a CRD's own defaulting (e.g. FRRConfiguration
// defaulting neighbors[].dualStackAddressFamily) adds fields we never set, and
// an exact comparison against that would never match, rewriting the object on
// every reconcile.
func TestSpecEqual_IgnoresFieldsOnlyExistingSets(t *testing.T) {
	existing := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"nodeSelector":    map[string]interface{}{"bgp_router": "true"},
			"serverDefaulted": "value-we-never-set",
		},
	}}
	desired := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"nodeSelector": map[string]interface{}{"bgp_router": "true"},
		},
	}}
	if !specEqual(existing, desired) {
		t.Error("expected a field only present on existing to be ignored")
	}

	changed := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{
			"nodeSelector": map[string]interface{}{"bgp_router": "false"},
		},
	}}
	if specEqual(existing, changed) {
		t.Error("expected a field we do set to still be compared")
	}
}

func TestCreateOrUpdate_SkipsUpdateWhenExistingHasServerDefaultedField(t *testing.T) {
	ctx := context.Background()
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	// The fake client doesn't apply CRD defaulting, so this stands in for a
	// field the real API server would have added on its own.
	existing := newFRRConfigObject("bgp-cc-1", map[string]interface{}{LabelManagedBy: LabelManagedByVal}, "true")
	spec := existing.Object["spec"].(map[string]interface{})
	spec["serverDefaulted"] = "value-we-never-set"

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()

	before := &unstructured.Unstructured{}
	before.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-1", Namespace: FRRNamespace}, before); err != nil {
		t.Fatalf("get before: %v", err)
	}

	desired := newFRRConfigObject("bgp-cc-1", map[string]interface{}{LabelManagedBy: LabelManagedByVal}, "true")
	if err := createOrUpdate(ctx, c, desired); err != nil {
		t.Fatalf("createOrUpdate: %v", err)
	}

	after := &unstructured.Unstructured{}
	after.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-1", Namespace: FRRNamespace}, after); err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.GetResourceVersion() != before.GetResourceVersion() {
		t.Fatalf("expected no update when existing only has an extra server-defaulted field, resourceVersion changed from %q to %q",
			before.GetResourceVersion(), after.GetResourceVersion())
	}
}

func TestCreateOrUpdate_PreservesForeignLabelsAndAnnotationsOnUpdate(t *testing.T) {
	ctx := context.Background()
	s := testScheme()
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfiguration"), &unstructured.Unstructured{})
	s.AddKnownTypeWithName(FRRConfigurationGVK.GroupVersion().WithKind("FRRConfigurationList"), &unstructured.UnstructuredList{})

	existing := newFRRConfigObject("bgp-cc-1", map[string]interface{}{
		LabelManagedBy:     LabelManagedByVal,
		"foreign.io/owner": "someone-else",
	}, "true")
	existing.SetAnnotations(map[string]string{"foreign.io/note": "keep-me"})

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(existing).Build()

	// A real spec change forces createOrUpdate down the Update path, which is
	// where a wholesale metadata replacement would lose anything we didn't set.
	desired := newFRRConfigObject("bgp-cc-1", map[string]interface{}{LabelManagedBy: LabelManagedByVal}, "false")
	if err := createOrUpdate(ctx, c, desired); err != nil {
		t.Fatalf("createOrUpdate: %v", err)
	}

	after := &unstructured.Unstructured{}
	after.SetGroupVersionKind(FRRConfigurationGVK)
	if err := c.Get(ctx, types.NamespacedName{Name: "bgp-cc-1", Namespace: FRRNamespace}, after); err != nil {
		t.Fatalf("get after: %v", err)
	}
	if after.GetLabels()["foreign.io/owner"] != "someone-else" {
		t.Errorf("expected foreign label to survive an update, got labels %v", after.GetLabels())
	}
	if after.GetAnnotations()["foreign.io/note"] != "keep-me" {
		t.Errorf("expected foreign annotation to survive an update, got annotations %v", after.GetAnnotations())
	}
}

func TestLabelsSatisfied(t *testing.T) {
	existing := map[string]string{LabelManagedBy: LabelManagedByVal, "foreign.io/owner": "someone-else"}
	desired := map[string]string{LabelManagedBy: LabelManagedByVal}
	if !labelsSatisfied(existing, desired) {
		t.Error("expected desired labels already present (plus a foreign label) to be satisfied")
	}

	if labelsSatisfied(map[string]string{}, desired) {
		t.Error("expected missing managed label to be unsatisfied")
	}

	if !labelsSatisfied(existing, nil) {
		t.Error("expected no desired labels to always be satisfied")
	}
}
