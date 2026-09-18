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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/bgp-cloud-connector/internal/controller"
)

const (
	// caBundleKey is the ConfigMap key the Cluster Network Operator populates
	// with the trusted CA bundle.
	caBundleKey = "ca-bundle.crt"

	// configMapName is the trusted CA ConfigMap shipped in the bundle and
	// mounted into the manager.
	configMapName = "openshift-bgp-cloud-connector-trusted-ca"

	// defaultPollInterval is how often the trusted CA bundle is re-read.
	defaultPollInterval = 2 * time.Minute
)

// +kubebuilder:rbac:groups="",resources=configmaps,resourceNames=openshift-bgp-cloud-connector-trusted-ca,verbs=get,namespace=openshift-bgp-cloud-connector

// Watcher polls the trusted CA ConfigMap and calls onChange when its bundle
// changes from the content observed at startup. It is a manager Runnable.
type Watcher struct {
	client      ctrlclient.Client
	initialHash string
	interval    time.Duration
	onChange    func()
	log         logr.Logger
}

// New reads the trusted CA ConfigMap's current bundle so a later change can be
// detected, and returns a Watcher for it.
// The caller passes a client that reads straight from the API server -- the
// manager's cache is not running yet, and a live GET needs no cache anyway.
func New(ctx context.Context, c ctrlclient.Client, onChange func()) (*Watcher, error) {
	w := &Watcher{
		client:   c,
		interval: defaultPollInterval,
		onChange: onChange,
		log:      logr.FromContextOrDiscard(ctx),
	}

	w.log.Info("Initializing trusted CA configmap watcher")

	var err error
	if w.initialHash, err = w.currentHash(ctx); err != nil {
		return nil, err
	}

	return w, nil
}

// SetupWithManager registers the Watcher so the manager runs and stops it.
func (w *Watcher) SetupWithManager(mgr ctrl.Manager) error {
	return mgr.Add(w)
}

// Start polls the trusted CA ConfigMap until its bundle changes or ctx is
// cancelled. It satisfies manager.Runnable.
func (w *Watcher) Start(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	w.log.Info("Starting to watch trusted CA configmap")

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			hash, err := w.currentHash(ctx)
			if err != nil {
				w.log.Error(err, "failed to read trusted CA configmap, will retry")
				continue
			}
			if hash != w.initialHash {
				w.log.Info("trusted CA bundle changed, initiating shutdown to reload it")
				w.onChange()
				return nil
			}
		}
	}
}

// currentHash returns a hash of the trusted CA ConfigMap's bundle, or the empty
// string when the ConfigMap does not exist yet.
func (w *Watcher) currentHash(ctx context.Context) (string, error) {
	cm := &corev1.ConfigMap{}
	if err := w.client.Get(ctx, types.NamespacedName{Namespace: controller.DefaultOperatorNamespace, Name: configMapName}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to read trusted CA configmap %s/%s: %w", controller.DefaultOperatorNamespace, configMapName, err)
	}
	return hashBundle(cm), nil
}

// hashBundle returns a hash of the ConfigMap's ca-bundle.crt so a change to the
// trusted CA content can be detected. Only that key matters for trust.
func hashBundle(cm *corev1.ConfigMap) string {
	sum := sha256.Sum256([]byte(cm.Data[caBundleKey]))
	return hex.EncodeToString(sum[:])
}
