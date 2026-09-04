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

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

// configStatusEqual reports whether two Config status values are semantically equal.
func configStatusEqual(a, b networkingapi.BGPCloudConfigurationStatus) bool {
	return apiequality.Semantic.DeepEqual(a, b)
}

// routingStatusEqual reports whether two Routing status values are semantically equal.
func routingStatusEqual(a, b networkingapi.BGPRoutingStatus) bool {
	return apiequality.Semantic.DeepEqual(a, b)
}

// patchConfigStatus updates status when desired differs from the etcd baseline.
// baselineStatus must be a DeepCopy of status as read from the API server at reconcile start.
func (r *BGPCloudConfigurationReconciler) patchConfigStatus(
	ctx context.Context,
	config *networkingapi.BGPCloudConfiguration,
	baselineStatus networkingapi.BGPCloudConfigurationStatus,
	mutate func(*networkingapi.BGPCloudConfiguration),
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
func (r *BGPRoutingReconciler) patchRoutingStatus(
	ctx context.Context,
	routing *networkingapi.BGPRouting,
	baselineStatus networkingapi.BGPRoutingStatus,
	mutate func(*networkingapi.BGPRouting),
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
func (r *BGPCloudConfigurationReconciler) reportDeletionBlocked(
	ctx context.Context,
	config *networkingapi.BGPCloudConfiguration,
	baselineStatus networkingapi.BGPCloudConfigurationStatus,
	routings []networkingapi.BGPRouting,
) error {
	names := make([]string, len(routings))
	for i := range routings {
		names[i] = routings[i].Name
	}
	sort.Strings(names)

	condMessage := fmt.Sprintf("%d BGPRouting CR(s) must be deleted first: %s",
		len(names), strings.Join(names, ", "))

	return r.patchConfigStatus(ctx, config, baselineStatus, func(c *networkingapi.BGPCloudConfiguration) {
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
			Type:               ConditionDeletionBlocked,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonRoutingCRsExist,
			Message:            condMessage,
			ObservedGeneration: c.Generation,
		})
	})
}
