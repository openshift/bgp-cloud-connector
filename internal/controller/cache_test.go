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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// The operator reads exactly one secret and is granted get on exactly
// that name. A cached read cannot be name-scoped -- serving it needs a
// list and a watch over the whole namespace, and resourceNames does not
// apply to collection verbs -- so the secret has to come from the API
// server directly or the Role has to be widened to match the informer.
func TestClientOptions_DoesNotCacheSecrets(t *testing.T) {
	cacheOpts := ClientOptions().Cache
	if cacheOpts == nil {
		t.Fatal("the client caches everything; reading the secret would need list and watch")
	}
	for _, obj := range cacheOpts.DisableFor {
		if _, ok := obj.(*corev1.Secret); ok {
			return
		}
	}
	t.Errorf("secrets are still cached: DisableFor is %v", cacheOpts.DisableFor)
}

// The operator only needs a small labelled set of FRR Pods. Reading it live
// avoids retaining every Pod in the cluster just to serve that list.
func TestClientOptions_DoesNotCachePods(t *testing.T) {
	cacheOpts := ClientOptions().Cache
	for _, obj := range cacheOpts.DisableFor {
		if _, ok := obj.(*corev1.Pod); ok {
			return
		}
	}
	t.Errorf("pods are still cached: DisableFor is %v", cacheOpts.DisableFor)
}

func TestClientOptions_CachesEverythingElse(t *testing.T) {
	disabled := ClientOptions().Cache.DisableFor
	if len(disabled) != 2 || reflect.TypeOf(disabled[0]) != reflect.TypeOf(&corev1.Secret{}) ||
		reflect.TypeOf(disabled[1]) != reflect.TypeOf(&corev1.Pod{}) {
		t.Fatalf("DisableFor = %v, want only Secret and Pod", disabled)
	}
}

func TestCacheOptions_OnlyWatchesVirtLauncherPods(t *testing.T) {
	byObject := CacheOptions().ByObject
	if len(byObject) != 1 {
		t.Fatalf("ByObject has %d entries, want only Pod", len(byObject))
	}
	for object, options := range byObject {
		if _, ok := object.(*corev1.Pod); !ok || options.Label == nil ||
			!options.Label.Matches(labels.Set{"kubevirt.io": "virt-launcher"}) ||
			options.Label.Matches(labels.Set{"app": "unrelated"}) {
			t.Fatalf("Pod cache selector = %v for %T", options.Label, object)
		}
	}
}
