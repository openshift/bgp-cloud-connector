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
	"fmt"
	"sort"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

// configStatusEqual reports whether two Config status values are semantically equal.
func configStatusEqual(a, b networkingv1alpha1.CUDNBgpConfigStatus) bool {
	return apiequality.Semantic.DeepEqual(a, b)
}

// routingStatusEqual reports whether two Routing status values are semantically equal.
func routingStatusEqual(a, b networkingv1alpha1.CUDNBgpRoutingStatus) bool {
	return apiequality.Semantic.DeepEqual(a, b)
}

// patchConfigStatus updates status when desired differs from the etcd baseline.
// baselineStatus must be a DeepCopy of status as read from the API server at reconcile start.
func (r *CUDNBgpConfigReconciler) patchConfigStatus(
	ctx context.Context,
	config *networkingv1alpha1.CUDNBgpConfig,
	baselineStatus networkingv1alpha1.CUDNBgpConfigStatus,
	mutate func(*networkingv1alpha1.CUDNBgpConfig),
) error {
	desired := config.DeepCopy()
	mutate(desired)

	if configStatusEqual(baselineStatus, desired.Status) {
		// Skip Status().Update when desired status matches etcd to avoid hot-loop writes.
		config.Status = desired.Status
		return nil
	}

	config.Status = desired.Status
	return r.Status().Update(ctx, config)
}

// patchRoutingStatus updates status when desired differs from the etcd baseline.
// baselineStatus must be a DeepCopy of status as read from the API server at reconcile start.
func (r *CUDNBgpRoutingReconciler) patchRoutingStatus(
	ctx context.Context,
	routing *networkingv1alpha1.CUDNBgpRouting,
	baselineStatus networkingv1alpha1.CUDNBgpRoutingStatus,
	mutate func(*networkingv1alpha1.CUDNBgpRouting),
) error {
	desired := routing.DeepCopy()
	mutate(desired)

	if routingStatusEqual(baselineStatus, desired.Status) {
		// Skip Status().Update when desired status matches etcd to avoid hot-loop writes.
		routing.Status = desired.Status
		return nil
	}

	routing.Status = desired.Status
	return r.Status().Update(ctx, routing)
}

// reportDeletionBlocked sets DeletionBlocked condition when routing CRs block config deletion.
func (r *CUDNBgpConfigReconciler) reportDeletionBlocked(
	ctx context.Context,
	config *networkingv1alpha1.CUDNBgpConfig,
	baselineStatus networkingv1alpha1.CUDNBgpConfigStatus,
	routings []networkingv1alpha1.CUDNBgpRouting,
) error {
	names := make([]string, len(routings))
	for i := range routings {
		names[i] = routings[i].Name
	}
	sort.Strings(names)

	condMessage := fmt.Sprintf("%d CUDNBgpRouting CR(s) must be deleted first: %s",
		len(names), strings.Join(names, ", "))

	return r.patchConfigStatus(ctx, config, baselineStatus, func(c *networkingv1alpha1.CUDNBgpConfig) {
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
			Type:               ConditionDeletionBlocked,
			Status:             metav1.ConditionTrue,
			Reason:             "RoutingCRsExist",
			Message:            condMessage,
			ObservedGeneration: c.Generation,
		})
	})
}
