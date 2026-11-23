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
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

// ForwardingLabel marks the workers the forwarding specs select with
// forwardingNodeSelector. The worker role alone would also select OSD's
// infra nodes, which the data-plane check never reaches, so the specs
// would change a setting on nodes they do not check.
const ForwardingLabel = "bgp-cloud-connector.openshift.io/e2e-forwarding"

// LabelForwardingWorkers puts ForwardingLabel on every node Workers
// returns and returns the selector that matches them.
func LabelForwardingWorkers(ctx context.Context, c client.Client) (map[string]string, error) {
	workers, err := Workers(ctx, c)
	if err != nil {
		return nil, err
	}
	for i := range workers {
		node := &workers[i]
		if _, ok := node.Labels[ForwardingLabel]; ok {
			continue
		}
		patch := client.MergeFrom(node.DeepCopy())
		node.Labels[ForwardingLabel] = ""
		if err := c.Patch(ctx, node, patch); err != nil {
			return nil, fmt.Errorf("labelling worker %s: %w", node.Name, err)
		}
	}
	return map[string]string{ForwardingLabel: ""}, nil
}

// RemoveForwardingLabel takes ForwardingLabel off every node that has it.
func RemoveForwardingLabel(ctx context.Context, c client.Client) error {
	nodes := &corev1.NodeList{}
	if err := c.List(ctx, nodes, client.HasLabels{ForwardingLabel}); err != nil {
		return err
	}
	var errs []error
	for i := range nodes.Items {
		node := &nodes.Items[i]
		patch := client.MergeFrom(node.DeepCopy())
		delete(node.Labels, ForwardingLabel)
		if err := c.Patch(ctx, node, patch); err != nil {
			errs = append(errs, fmt.Errorf("unlabelling node %s: %w", node.Name, err))
		}
	}
	return errors.Join(errs...)
}

// ClearForwardingSelector unsets forwardingNodeSelector on the named
// configuration and returns the generation the change produced. The
// operator never turns forwarding off, so a spec that wants it off again
// has to stop the operator selecting those nodes first.
func ClearForwardingSelector(ctx context.Context, c client.Client, name string) (int64, error) {
	cfg := &networkingapi.BGPCloudConfiguration{}
	if err := c.Get(ctx, types.NamespacedName{Name: name}, cfg); err != nil {
		return 0, err
	}
	patch := client.MergeFrom(cfg.DeepCopy())
	cfg.Spec.ForwardingNodeSelector = nil
	if err := c.Patch(ctx, cfg, patch); err != nil {
		return 0, err
	}
	return cfg.Generation, nil
}

// CloudPassDone returns nil once CloudResourcesReconciled is true at
// generation or later. The controller sets it only after ReconcileNodes
// returns, so a nil result means a whole cloud pass saw that generation.
func CloudPassDone(ctx context.Context, c client.Client, name string, generation int64) error {
	cfg := &networkingapi.BGPCloudConfiguration{}
	if err := c.Get(ctx, types.NamespacedName{Name: name}, cfg); err != nil {
		return err
	}
	cond := meta.FindStatusCondition(cfg.Status.Conditions, networkingapi.ConditionCloudResourcesReconciled)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.ObservedGeneration < generation {
		return fmt.Errorf("no completed cloud pass at generation %d yet: %+v", generation, cond)
	}
	return nil
}
