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
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

// setSummaryConditions assigns phase and derives the Available/Progressing/
// Degraded summary conditions from it, so a caller sets both through one call
// and the four can never drift apart. The summary conditions sit alongside the
// granular per-step conditions and give cluster tooling a single health signal:
// `kubectl wait --for=condition=Available` and the Available printcolumn both
// read them. The reason follows the OpenShift ClusterOperator convention, where
// a healthy condition reads AsExpected.
//
// reason and message describe the current situation. Available always carries
// them, because it is the summary surfaced in the printcolumn and must show the
// cause when the resource is not available. Progressing and Degraded each carry
// the detail only when they are the axis in effect, and otherwise read
// AsExpected with no message — echoing the same message onto every False row
// reads as noise. reason and message must be stable across a settled reconcile,
// or the baseline-diff that keeps status writes off the hot loop is defeated.
func setSummaryConditions(
	phase *networkingapi.PhaseType,
	conds *[]metav1.Condition,
	generation int64,
	target networkingapi.PhaseType,
	reason, message string,
) {
	*phase = target

	// Available mirrors the current state and always carries the detail.
	available := metav1.Condition{
		Type: ConditionAvailable, Status: metav1.ConditionFalse,
		Reason: reason, Message: message, ObservedGeneration: generation,
	}
	// Progressing and Degraded default to their healthy, detail-free form and
	// take the detail only in the branch that turns them on.
	progressing := metav1.Condition{
		Type: ConditionProgressing, Status: metav1.ConditionFalse,
		Reason: ReasonAsExpected, ObservedGeneration: generation,
	}
	degraded := metav1.Condition{
		Type: ConditionDegraded, Status: metav1.ConditionFalse,
		Reason: ReasonAsExpected, ObservedGeneration: generation,
	}

	switch target {
	case networkingapi.PhaseReady:
		available.Status = metav1.ConditionTrue
	case networkingapi.PhaseDegraded:
		degraded.Status = metav1.ConditionTrue
		degraded.Reason = reason
		degraded.Message = message
	default: // Pending, Configuring: work is still in flight.
		progressing.Status = metav1.ConditionTrue
		progressing.Reason = reason
		progressing.Message = message
	}

	for _, c := range []metav1.Condition{available, progressing, degraded} {
		meta.SetStatusCondition(conds, c)
	}
}

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

// persistNetworkOwnership writes the two Network/cluster ownership fields on
// their own, as soon as the patch that earned them succeeded.
func (r *BGPCloudConfigurationReconciler) persistNetworkOwnership(ctx context.Context, config *networkingapi.BGPCloudConfiguration) error {
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"resourceVersion": config.ResourceVersion,
		},
		"status": map[string]interface{}{
			"frrProviderOwnership":         config.Status.FRRProviderOwnership,
			"routeAdvertisementsOwnership": config.Status.RouteAdvertisementsOwnership,
		},
	})
	if err != nil {
		return err
	}
	inProgress := config.Status
	defer func() { config.Status = inProgress }()
	return r.Status().Patch(ctx, config, client.RawPatch(types.MergePatchType, patch))
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

	condMessage := truncateConditionMessage(fmt.Sprintf("%d BGPRouting CR(s) must be deleted first: %s",
		len(names), strings.Join(names, ", ")))

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
