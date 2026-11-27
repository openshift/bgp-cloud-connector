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
	"crypto/sha256"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

// OVN-Kubernetes copies CUDN labels to its NADs, which RouteAdvertisements
// selects by label. Use the CUDN UID as the opt-in value so a replacement
// network of the same name requires its own explicit opt-in.
const labelRoutingNetworkRef = "networking.openshift.io/bgp-network-ref"

const (
	layer2Topology     = "Layer2"
	layer3Topology     = "Layer3"
	primaryNetworkRole = "Primary"
)

func cudnOptedInForReference(cudn client.Object) bool {
	return cudn.GetUID() != "" && cudn.GetLabels()[labelRoutingNetworkRef] == string(cudn.GetUID())
}

func (r *BGPRoutingReconciler) reconcileNetworkRef(ctx context.Context, routing *networkingapi.BGPRouting, baseline networkingapi.BGPRoutingStatus) (ctrl.Result, error) {
	cudn := &unstructured.Unstructured{}
	cudn.SetGroupVersionKind(ClusterUDNGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: routing.Spec.NetworkRef.Name}, cudn); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		routing.Status.NetworkRef = nil
		return r.rejectNetworkReference(ctx, routing, baseline, "NetworkNotFound",
			fmt.Sprintf("ClusterUserDefinedNetwork %q does not exist", routing.Spec.NetworkRef.Name))
	}
	network := resolvedRoutingNetwork(cudn)
	routing.Status.NetworkRef = network

	switch {
	case !cudn.GetDeletionTimestamp().IsZero():
		return r.rejectNetworkReference(ctx, routing, baseline, "NetworkDeleting", "The referenced CUDN is being deleted")
	case network.Role != primaryNetworkRole || (network.Topology != layer2Topology && network.Topology != layer3Topology):
		return r.rejectNetworkReference(ctx, routing, baseline, "UnsupportedNetwork", "The referenced CUDN must be a primary Layer2 or Layer3 network")
	case cudn.GetLabels()[LabelAdvertise] == "true":
		return r.rejectNetworkReference(ctx, routing, baseline, "ConflictingAdvertisement", "The referenced CUDN has the label used by the shared BGPRouting advertisement")
	case !cudnOptedInForReference(cudn):
		return r.rejectNetworkReference(ctx, routing, baseline, "OptInRequired",
			fmt.Sprintf("Set the CUDN label %s to its UID", labelRoutingNetworkRef))
	}

	matching := &unstructured.UnstructuredList{}
	matching.SetGroupVersionKind(ClusterUDNGVK.GroupVersion().WithKind("ClusterUserDefinedNetworkList"))
	if err := r.List(ctx, matching, client.MatchingLabels{labelRoutingNetworkRef: string(cudn.GetUID())}); err != nil {
		return ctrl.Result{}, err
	}
	for _, candidate := range matching.Items {
		if candidate.GetUID() != cudn.GetUID() {
			return r.rejectNetworkReference(ctx, routing, baseline, "ConflictingSelector",
				fmt.Sprintf("CUDN %q carries the same advertisement opt-in label", candidate.GetName()))
		}
	}
	setReferenceCondition(routing, networkingapi.ConditionNetworkResolved, metav1.ConditionTrue, "Resolved", "The referenced primary CUDN is eligible for advertisement")

	config := &networkingapi.BGPCloudConfiguration{}
	err := r.Get(ctx, client.ObjectKey{Name: SingletonName}, config)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if config.Status.Phase != networkingapi.PhaseReady || !config.DeletionTimestamp.IsZero() {
		return r.reportReferencedAdvertisement(ctx, routing, baseline, networkingapi.PhasePending, metav1.ConditionUnknown,
			"WaitingForPeering", "Waiting for BGPCloudConfiguration to be ready")
	}

	ra, err := r.ensureReferencedAdvertisement(ctx, routing, cudn)
	if err != nil {
		routing.Status.RouteAdvertisementsRef = nil
		return r.reportReferencedAdvertisement(ctx, routing, baseline, networkingapi.PhaseDegraded, metav1.ConditionUnknown,
			ReasonRAFailed, err.Error())
	}
	routing.Status.RouteAdvertisementsRef = &networkingapi.RoutingAdvertisementReference{Name: ra.GetName()}
	return r.observeReferencedAdvertisement(ctx, routing, baseline, ra)
}

func (r *BGPRoutingReconciler) rejectNetworkReference(ctx context.Context, routing *networkingapi.BGPRouting, baseline networkingapi.BGPRoutingStatus, reason, message string) (ctrl.Result, error) {
	if err := r.deleteReferencedAdvertisement(ctx, routing); err != nil {
		return ctrl.Result{}, err
	}
	routing.Status.RouteAdvertisementsRef = nil
	setReferenceCondition(routing, networkingapi.ConditionNetworkResolved, metav1.ConditionFalse, reason, message)
	return r.reportReferencedAdvertisement(ctx, routing, baseline, networkingapi.PhaseDegraded, metav1.ConditionFalse,
		"NetworkUnresolved", "The referenced network is unavailable for advertisement")
}

// A single name per CUDN lets the API server arbitrate competing BGPRouting
// resources without either reconciler rewriting or deleting the other's policy.
func referencedAdvertisementName(networkName string) string {
	sum := sha256.Sum256([]byte(networkName))
	return fmt.Sprintf("bgp-cc-ref-%x", sum[:16])
}

func (r *BGPRoutingReconciler) ensureReferencedAdvertisement(ctx context.Context, routing *networkingapi.BGPRouting, cudn *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	desired := routeAdvertisements(referencedAdvertisementName(cudn.GetName()),
		map[string]interface{}{labelRoutingNetworkRef: string(cudn.GetUID())},
		map[string]interface{}{"matchLabels": map[string]interface{}{LabelManagedBy: LabelManagedByVal}})
	ra := &unstructured.Unstructured{}
	ra.SetGroupVersionKind(RouteAdvertisementsGVK)
	ra.SetName(desired.GetName())
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ra, func() error {
		if ra.GetResourceVersion() != "" && !referencedAdvertisementOwnedBy(ra, routing) {
			return fmt.Errorf("RouteAdvertisements %q is already owned by another resource", ra.GetName())
		}
		if ra.GetResourceVersion() == "" {
			controller := true
			ra.SetOwnerReferences([]metav1.OwnerReference{{
				APIVersion: networkingapi.GroupVersion.String(), Kind: "BGPRouting",
				Name: routing.Name, UID: routing.UID, Controller: &controller,
			}})
		}
		ra.Object["spec"] = desired.Object["spec"]
		ra.SetLabels(mergeLabels(ra.GetLabels(), desired.GetLabels()))
		return nil
	})
	return ra, err
}

func referencedAdvertisementOwnedBy(ra *unstructured.Unstructured, routing *networkingapi.BGPRouting) bool {
	owner := metav1.GetControllerOfNoCopy(ra)
	return owner != nil && owner.APIVersion == networkingapi.GroupVersion.String() &&
		owner.Kind == "BGPRouting" && owner.Name == routing.Name && owner.UID == routing.UID
}

func (r *BGPRoutingReconciler) deleteReferencedAdvertisement(ctx context.Context, routing *networkingapi.BGPRouting) error {
	ra := &unstructured.Unstructured{}
	ra.SetGroupVersionKind(RouteAdvertisementsGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: referencedAdvertisementName(routing.Spec.NetworkRef.Name)}, ra); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return client.IgnoreNotFound(err)
	}
	if !referencedAdvertisementOwnedBy(ra, routing) {
		return nil
	}
	uid, version := ra.GetUID(), ra.GetResourceVersion()
	return client.IgnoreNotFound(r.Delete(ctx, ra, client.Preconditions{UID: &uid, ResourceVersion: &version}))
}

func (r *BGPRoutingReconciler) observeReferencedAdvertisement(ctx context.Context, routing *networkingapi.BGPRouting, baseline networkingapi.BGPRoutingStatus, ra *unstructured.Unstructured) (ctrl.Result, error) {
	var status struct {
		Conditions []metav1.Condition `json:"conditions"`
	}
	raw, _, err := unstructured.NestedMap(ra.Object, "status")
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &status); err != nil {
		return ctrl.Result{}, err
	}
	accepted := meta.FindStatusCondition(status.Conditions, "Accepted")
	if !ra.GetDeletionTimestamp().IsZero() || accepted == nil ||
		accepted.ObservedGeneration != ra.GetGeneration() || accepted.Status == metav1.ConditionUnknown {
		return r.reportReferencedAdvertisement(ctx, routing, baseline, networkingapi.PhaseConfiguring, metav1.ConditionUnknown,
			"WaitingForOVN", "Waiting for OVN-Kubernetes to observe the current RouteAdvertisements generation")
	}
	if accepted.Status == metav1.ConditionFalse {
		phase := networkingapi.PhaseDegraded
		if accepted.Reason == "ConfigurationPending" {
			phase = networkingapi.PhaseConfiguring
		}
		return r.reportReferencedAdvertisement(ctx, routing, baseline, phase, accepted.Status, accepted.Reason, accepted.Message)
	}
	degraded := meta.FindStatusCondition(status.Conditions, "Degraded")
	if degraded != nil && degraded.Status == metav1.ConditionTrue && degraded.ObservedGeneration == ra.GetGeneration() {
		setReferenceCondition(routing, "RouteAdvertisementsDegraded", metav1.ConditionTrue, degraded.Reason, degraded.Message)
		return r.reportReferencedAdvertisement(ctx, routing, baseline, networkingapi.PhaseDegraded, accepted.Status, accepted.Reason, accepted.Message)
	}
	return r.reportReferencedAdvertisement(ctx, routing, baseline, networkingapi.PhaseReady, accepted.Status, accepted.Reason, accepted.Message)
}

func (r *BGPRoutingReconciler) reportReferencedAdvertisement(ctx context.Context, routing *networkingapi.BGPRouting, baseline networkingapi.BGPRoutingStatus, phase networkingapi.PhaseType, status metav1.ConditionStatus, reason, message string) (ctrl.Result, error) {
	if phase != networkingapi.PhaseDegraded || status != metav1.ConditionTrue {
		meta.RemoveStatusCondition(&routing.Status.Conditions, "RouteAdvertisementsDegraded")
	}
	setReferenceCondition(routing, networkingapi.ConditionRouteAdvertisementsAccepted, status, reason, message)
	if phase == networkingapi.PhaseReady {
		reason = ReasonAsExpected
	} else if degraded := meta.FindStatusCondition(routing.Status.Conditions, "RouteAdvertisementsDegraded"); degraded != nil {
		reason, message = degraded.Reason, degraded.Message
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, r.patchRoutingStatus(ctx, routing, baseline, func(rt *networkingapi.BGPRouting) {
		setSummaryConditions(&rt.Status.Phase, &rt.Status.Conditions, rt.Generation, phase, reason, truncateConditionMessage(message))
	})
}

func setReferenceCondition(routing *networkingapi.BGPRouting, kind string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&routing.Status.Conditions, metav1.Condition{
		Type: kind, Status: status, Reason: reason, Message: truncateConditionMessage(message),
		ObservedGeneration: routing.Generation,
	})
}
