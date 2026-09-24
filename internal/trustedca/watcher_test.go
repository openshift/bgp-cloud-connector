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

package trustedca

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace = "openshift-bgp-cloud-connector"
	testName      = "openshift-bgp-cloud-connector-trusted-ca"
)

func TestCurrentHash_MissingConfigMapIsEmpty(t *testing.T) {
	w := &Watcher{
		client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build(),
	}

	hash, err := w.currentHash(context.Background())
	if err != nil {
		t.Fatalf("currentHash returned error for missing configmap: %v", err)
	}
	if hash != "" {
		t.Errorf("expected empty baseline for missing configmap, got %q", hash)
	}
}

func TestCurrentHash_PresentMatchesBundle(t *testing.T) {
	cm := caConfigMap("original-bundle")
	w := &Watcher{
		client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cm).Build(),
	}

	hash, err := w.currentHash(context.Background())
	if err != nil {
		t.Fatalf("currentHash: %v", err)
	}
	if want := hashBundle(cm); hash != want {
		t.Errorf("hash = %q, want %q", hash, want)
	}
}

func TestStart_TriggersWhenBundleChanges(t *testing.T) {
	cm := caConfigMap("rotated-bundle") // client already holds the rotated bundle
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cm)

	triggered := make(chan struct{})
	w := testWatcher(c, "original-bundle", func() { close(triggered) })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case <-triggered:
	case <-ctx.Done():
		t.Fatal("onChange did not fire although the bundle rotated")
	}
}

func TestStart_DoesNotTriggerWhenUnchanged(t *testing.T) {
	cm := caConfigMap("original-bundle")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cm)

	var called atomic.Bool
	w := testWatcher(c, "original-bundle", func() { called.Store(true) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if called.Load() {
		t.Error("onChange fired although the bundle was unchanged")
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to build scheme: %v", err)
	}
	return scheme
}

func caConfigMap(bundle string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName},
		Data:       map[string]string{caBundleKey: bundle},
	}
}

func testWatcher(c *fake.ClientBuilder, baseline string, onChange func()) *Watcher {
	return &Watcher{
		client:      c.Build(),
		initialHash: hashBundle(caConfigMap(baseline)),
		interval:    time.Millisecond,
		onChange:    onChange,
		log:         logr.Discard(),
	}
}
