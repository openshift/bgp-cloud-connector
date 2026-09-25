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
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	toolscache "k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

func TestSetupWithManagerUsesPodEventsWhenKubeVirtIsAbsent(t *testing.T) {
	scheme := routingTestScheme()
	routing := newTestBGPRouting()
	namespace := testVMNamespace()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(routing, namespace).Build()
	cache := &setupTestCache{}
	mgr := &setupTestManager{
		scheme: scheme,
		mapper: meta.NewDefaultRESTMapper(nil),
		cache:  cache,
	}
	r := &BGPRoutingReconciler{Client: c, Scheme: scheme}
	if err := r.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup controller: %v", err)
	}
	if mgr.runnable == nil {
		t.Fatal("setup did not register a controller")
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- mgr.runnable.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-errCh; err != nil {
			t.Errorf("stop controller: %v", err)
		}
	})

	if !waitFor(2*time.Second, func() bool { return cache.handlerCount(&corev1.Pod{}) > 0 }) {
		t.Fatal("Pod fallback watch was not registered")
	}
	cache.add(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "virt-launcher", Namespace: namespace.Name}})

	if !waitFor(2*time.Second, func() bool {
		updated := newTestBGPRouting()
		if err := c.Get(ctx, client.ObjectKeyFromObject(routing), updated); err != nil {
			return false
		}
		return len(updated.Finalizers) == 1 && updated.Finalizers[0] == RoutingFinalizerName
	}) {
		t.Fatal("virt-launcher Pod event did not enqueue its namespace's BGPRouting")
	}
}

func waitFor(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return condition()
}

type setupTestManager struct {
	manager.Manager
	scheme   *runtime.Scheme
	mapper   meta.RESTMapper
	cache    crcache.Cache
	runnable manager.Runnable
}

func (m *setupTestManager) GetScheme() *runtime.Scheme              { return m.scheme }
func (m *setupTestManager) GetRESTMapper() meta.RESTMapper          { return m.mapper }
func (m *setupTestManager) GetCache() crcache.Cache                 { return m.cache }
func (m *setupTestManager) GetLogger() logr.Logger                  { return logr.Discard() }
func (m *setupTestManager) GetControllerOptions() config.Controller { return config.Controller{} }
func (m *setupTestManager) Add(runnable manager.Runnable) error {
	m.runnable = runnable
	return nil
}

type setupTestCache struct {
	crcache.Cache
	mu       sync.Mutex
	handlers map[reflect.Type][]toolscache.ResourceEventHandler
}

func (c *setupTestCache) GetInformer(_ context.Context, obj client.Object, _ ...crcache.InformerGetOption) (crcache.Informer, error) {
	return &setupTestInformer{cache: c, objectType: reflect.TypeOf(obj)}, nil
}

func (c *setupTestCache) WaitForCacheSync(context.Context) bool { return true }

func (c *setupTestCache) handlerCount(obj client.Object) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.handlers[reflect.TypeOf(obj)])
}

func (c *setupTestCache) add(obj client.Object) {
	c.mu.Lock()
	handlers := append([]toolscache.ResourceEventHandler(nil), c.handlers[reflect.TypeOf(obj)]...)
	c.mu.Unlock()
	for _, handler := range handlers {
		handler.OnAdd(obj, false)
	}
}

type setupTestInformer struct {
	crcache.Informer
	cache      *setupTestCache
	objectType reflect.Type
}

func (i *setupTestInformer) AddEventHandlerWithOptions(handler toolscache.ResourceEventHandler, _ toolscache.HandlerOptions) (toolscache.ResourceEventHandlerRegistration, error) {
	i.cache.mu.Lock()
	defer i.cache.mu.Unlock()
	if i.cache.handlers == nil {
		i.cache.handlers = make(map[reflect.Type][]toolscache.ResourceEventHandler)
	}
	i.cache.handlers[i.objectType] = append(i.cache.handlers[i.objectType], handler)
	return setupTestRegistration{}, nil
}

type setupTestRegistration struct {
	toolscache.ResourceEventHandlerRegistration
}

func (setupTestRegistration) HasSynced() bool { return true }
