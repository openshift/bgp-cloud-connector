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

package e2e

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
)

// adoptable is anything a suite creates from a manifest and may adopt
// from a previous run instead.
type adoptable = runtime.Object

// CheckAdopted reports whether stored, an object a previous run left on
// the cluster, is the object the manifest describes. Every field the
// manifest sets under spec, and every label it sets, must have the same
// value in stored.
//
// Fields only stored has are ignored: the API server and defaulting fill
// those in, and the manifest never said anything about them. A field the
// manifest leaves at its zero value counts as not set, because a
// manifest decoded into a Go type cannot tell "absent" from "zero"; a
// manifest that sets a field to zero on purpose is therefore not checked
// on that field.
//
// The error names every field that differs, so a run adopting the wrong
// object says which profile it came from rather than failing later in a
// way that looks like a product bug.
func CheckAdopted(desired, stored adoptable) error {
	want, err := runtime.DefaultUnstructuredConverter.ToUnstructured(desired)
	if err != nil {
		return fmt.Errorf("converting the manifest: %w", err)
	}
	got, err := runtime.DefaultUnstructuredConverter.ToUnstructured(stored)
	if err != nil {
		return fmt.Errorf("converting the stored object: %w", err)
	}

	var diffs []string
	compare("spec", want["spec"], got["spec"], &diffs)
	compare("metadata.labels", nested(want, "metadata", "labels"), nested(got, "metadata", "labels"), &diffs)
	if len(diffs) == 0 {
		return nil
	}
	sort.Strings(diffs)
	return fmt.Errorf("the object on the cluster is not the one the manifest describes: %s",
		strings.Join(diffs, "; "))
}

// compare records, under path, every place where want sets a value that
// got does not have. Maps are compared key by key, so got may carry
// more; lists are compared element by element and must be the same
// length, because a list with a member added or removed is a different
// list.
func compare(path string, want, got any, diffs *[]string) {
	if isZero(want) {
		return
	}
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			*diffs = append(*diffs, fmt.Sprintf("%s is %v, want %v", path, got, want))
			return
		}
		for k, v := range w {
			compare(path+"."+k, v, g[k], diffs)
		}
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			*diffs = append(*diffs, fmt.Sprintf("%s is %v, want %v", path, got, want))
			return
		}
		for i := range w {
			compare(fmt.Sprintf("%s[%d]", path, i), w[i], g[i], diffs)
		}
	default:
		if !reflect.DeepEqual(want, got) {
			*diffs = append(*diffs, fmt.Sprintf("%s is %v, want %v", path, got, want))
		}
	}
}

func isZero(v any) bool {
	if v == nil {
		return true
	}
	switch t := v.(type) {
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	}
	return reflect.ValueOf(v).IsZero()
}

func nested(obj map[string]any, keys ...string) any {
	var cur any = obj
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}
