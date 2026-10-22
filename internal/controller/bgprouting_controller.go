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
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

// +kubebuilder:rbac:groups=networking.openshift.io,resources=bgproutings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.openshift.io,resources=bgproutings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.openshift.io,resources=bgproutings/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=clusteruserdefinednetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=routeadvertisements,verbs=get;list;watch;create;update;patch;delete

type BGPRoutingReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder emits Events on notable transitions. It is nil in unit tests
	// that construct the reconciler directly; emitEvent tolerates that.
	Recorder record.EventRecorder
}

// emitEvent records an Event, tolerating a nil Recorder so unit tests that omit
// it do not panic. Callers emit only on transitions, not every reconcile.
func (r *BGPRoutingReconciler) emitEvent(routing *networkingapi.BGPRouting, eventType, reason, message string) {
	if r.Recorder != nil {
		r.Recorder.Event(routing, eventType, reason, message)
	}
}

func (r *BGPRoutingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	routing := &networkingapi.BGPRouting{}
	if err := r.Get(ctx, req.NamespacedName, routing); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	baselineStatus := routing.Status.DeepCopy()

	if !routing.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, routing)
	}

	if !controllerutil.ContainsFinalizer(routing, RoutingFinalizerName) {
		controllerutil.AddFinalizer(routing, RoutingFinalizerName)
		if err := r.Update(ctx, routing); err != nil {
			return ctrl.Result{}, err
		}
	}

	routing.Status.Phase = networkingapi.PhaseConfiguring
	routing.Status.ObservedGeneration = routing.Generation

	// Pre-check: spec.network.name must be unique across all BGPRouting CRs
	routingList := &networkingapi.BGPRoutingList{}
	if err := r.List(ctx, routingList); err != nil {
		return ctrl.Result{}, err
	}
	for i := range routingList.Items {
		other := &routingList.Items[i]
		if other.Name != routing.Name && other.Spec.Network.Name == routing.Spec.Network.Name {
			return r.setDegraded(ctx, routing, *baselineStatus, networkingapi.ConditionNetworkCreated,
				ReasonDuplicateNetwork,
				fmt.Sprintf("spec.network.name %q already claimed by BGPRouting %q", routing.Spec.Network.Name, other.Name))
		}
	}

	// Pre-check: BGPCloudConfiguration must exist and be Ready
	bgpConfig := &networkingapi.BGPCloudConfiguration{}
	if err := r.Get(ctx, types.NamespacedName{Name: SingletonName}, bgpConfig); err != nil {
		if err := r.patchRoutingStatus(ctx, routing, *baselineStatus, func(rt *networkingapi.BGPRouting) {
			clearRoutingStepConditions(rt)
			rt.Status.Namespaces = nil
			setSummaryConditions(&rt.Status.Phase, &rt.Status.Conditions, rt.Generation,
				networkingapi.PhasePending, ReasonWaitingForConfig,
				"BGPCloudConfiguration \"cluster\" not found")
		}); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("BGPCloudConfiguration 'cluster' not found, requeueing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if bgpConfig.Status.Phase != networkingapi.PhaseReady {
		if err := r.patchRoutingStatus(ctx, routing, *baselineStatus, func(rt *networkingapi.BGPRouting) {
			clearRoutingStepConditions(rt)
			rt.Status.Namespaces = nil
			setSummaryConditions(&rt.Status.Phase, &rt.Status.Conditions, rt.Generation,
				networkingapi.PhasePending, ReasonWaitingForConfig,
				fmt.Sprintf("Waiting for BGPCloudConfiguration to become Ready (currently %q)", bgpConfig.Status.Phase))
		}); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("BGPCloudConfiguration not Ready, requeueing", "phase", bgpConfig.Status.Phase)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Phase 1: Validate namespace labels + ensure ClusterUDN
	log.Info("Phase 1: validating namespace and ensuring ClusterUDN", "network", routing.Spec.Network.Name)
	namespaces, err := MatchedNamespaces(ctx, r.Client, routing.Spec.Network.Name)
	if err != nil {
		return r.setDegraded(ctx, routing, *baselineStatus, networkingapi.ConditionNetworkCreated,
			ReasonNamespaceNotReady, fmt.Sprintf("namespace validation failed: %v", err))
	}
	routing.Status.Namespaces = namespaces
	if err := EnsureClusterUDN(ctx, r.Client, routing); err != nil {
		var validationErr *CUDNValidationError
		if errors.As(err, &validationErr) {
			return r.setDegraded(ctx, routing, *baselineStatus, networkingapi.ConditionNetworkCreated,
				ReasonCUDNSpecInvalid, validationErr.Error())
		}
		return r.setDegraded(ctx, routing, *baselineStatus, networkingapi.ConditionNetworkCreated,
			ReasonCUDNFailed, fmt.Sprintf("failed to ensure ClusterUDN: %v", err))
	}
	meta.SetStatusCondition(&routing.Status.Conditions, metav1.Condition{
		Type:   networkingapi.ConditionNetworkCreated,
		Status: metav1.ConditionTrue,
		Reason: ReasonCreated,
		Message: fmt.Sprintf("ClusterUDN %q ensured, selecting %s",
			ClusterUDNNamePrefix+routing.Spec.Network.Name, describeNamespaces(namespaces)),
		ObservedGeneration: routing.Generation,
	})

	// Phase 2: Ensure shared RouteAdvertisements
	log.Info("Phase 2: ensuring RouteAdvertisements")
	if err := EnsureRouteAdvertisements(ctx, r.Client); err != nil {
		return r.setDegraded(ctx, routing, *baselineStatus, networkingapi.ConditionRouteAdvertisementsCreated,
			ReasonRAFailed, fmt.Sprintf("failed to ensure RouteAdvertisements: %v", err))
	}
	meta.SetStatusCondition(&routing.Status.Conditions, metav1.Condition{
		Type:               networkingapi.ConditionRouteAdvertisementsCreated,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonCreated,
		Message:            "Shared RouteAdvertisements ensured",
		ObservedGeneration: routing.Generation,
	})

	readyMsg := fmt.Sprintf("Network is advertised via BGP to %s", describeNamespaces(namespaces))
	if err := r.patchRoutingStatus(ctx, routing, *baselineStatus, func(rt *networkingapi.BGPRouting) {
		setSummaryConditions(&rt.Status.Phase, &rt.Status.Conditions, rt.Generation,
			networkingapi.PhaseReady, ReasonAsExpected, readyMsg)
	}); err != nil {
		return ctrl.Result{}, err
	}

	if baselineStatus.Phase != networkingapi.PhaseReady {
		r.emitEvent(routing, corev1.EventTypeNormal, ReasonAsExpected,
			fmt.Sprintf("Network %q is advertised via BGP to %s", routing.Spec.Network.Name, describeNamespaces(namespaces)))
	}

	log.Info("reconciliation complete", "phase", routing.Status.Phase)
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *BGPRoutingReconciler) reconcileDelete(ctx context.Context, routing *networkingapi.BGPRouting) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Delete ClusterUDN (namespace is left intact)
	log.Info("deleting ClusterUDN", "network", routing.Spec.Network.Name)
	if err := DeleteClusterUDN(ctx, r.Client, routing.Spec.Network.Name); err != nil {
		return ctrl.Result{}, err
	}

	// Delete shared RouteAdvertisements only if this is the last BGPRouting
	routingList := &networkingapi.BGPRoutingList{}
	if err := r.List(ctx, routingList); err != nil {
		return ctrl.Result{}, err
	}

	remaining := 0
	for i := range routingList.Items {
		if routingList.Items[i].Name != routing.Name {
			remaining++
		}
	}

	if remaining == 0 {
		log.Info("last BGPRouting, deleting shared RouteAdvertisements")
		if err := DeleteRouteAdvertisements(ctx, r.Client); err != nil {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(routing, RoutingFinalizerName)
	if err := r.Update(ctx, routing); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("finalizer removed, deletion complete")
	return ctrl.Result{}, nil
}

// clearRoutingStepConditions drops the per-step conditions while leaving the
// aggregate summary conditions in place. Without a Ready config there is no
// network to have created, so a stale NetworkCreated=True would contradict the
// Pending summary; the summary conditions are kept so setSummaryConditions can
// update them in place and preserve LastTransitionTime, which is what keeps a
// steady wait from rewriting status every requeue.
func clearRoutingStepConditions(rt *networkingapi.BGPRouting) {
	meta.RemoveStatusCondition(&rt.Status.Conditions, networkingapi.ConditionNetworkCreated)
	meta.RemoveStatusCondition(&rt.Status.Conditions, networkingapi.ConditionRouteAdvertisementsCreated)
}

// describeNamespaces renders a matched namespace set for a status message or
// event. With a single member it names it directly, which is the common case
// and more useful than a count; with several it gives the count. The full list
// always lives in status.namespaces, so the message stays short either way.
func describeNamespaces(names []string) string {
	if len(names) == 1 {
		return fmt.Sprintf("namespace %q", names[0])
	}
	return fmt.Sprintf("%d namespaces", len(names))
}

func (r *BGPRoutingReconciler) setDegraded(
	ctx context.Context,
	routing *networkingapi.BGPRouting,
	baselineStatus networkingapi.BGPRoutingStatus,
	condType, reason, message string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	terminal := IsTerminalDegradedReason(reason)
	if terminal {
		log.Info("terminal degraded condition, not requeueing", "reason", reason, "message", message)
	} else {
		log.Error(fmt.Errorf("%s: %s", reason, message), "setting degraded status")
	}

	if err := r.patchRoutingStatus(ctx, routing, baselineStatus, func(rt *networkingapi.BGPRouting) {
		// The network is not Ready, so the reported namespace set no longer holds.
		rt.Status.Namespaces = nil
		setSummaryConditions(&rt.Status.Phase, &rt.Status.Conditions, rt.Generation,
			networkingapi.PhaseDegraded, reason, message)
		meta.SetStatusCondition(&rt.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: rt.Generation,
		})
	}); err != nil {
		return ctrl.Result{}, err
	}
	if baselineStatus.Phase != networkingapi.PhaseDegraded {
		r.emitEvent(routing, corev1.EventTypeWarning, reason, message)
	}
	if terminal {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// SetupWithManager watches only APIs that exist on every cluster the
// operator can be installed on. ClusterUserDefinedNetwork ships with
// OVN-Kubernetes and qualifies; RouteAdvertisements does not, since CNO
// creates that CRD only once BGPCloudConfiguration has asked for route
// advertisements. Drift on the RouteAdvertisements we write is picked up
// by ResyncInterval instead.
func (r *BGPRoutingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cudn := &unstructured.Unstructured{}
	cudn.SetGroupVersionKind(ClusterUDNGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingapi.BGPRouting{}).
		Watches(cudn, handler.EnqueueRequestsFromMapFunc(
			r.mapClusterUDNToRouting,
		)).
		// Watch namespaces so status.namespaces tracks label changes without
		// waiting for the 5-minute resync. Enqueue every routing (there are few,
		// one per network) so a label being *removed* re-reconciles the routing
		// that used to include it, which a per-object mapper reading the new
		// labels could not. The predicate keeps this off unrelated namespace
		// churn: only cluster-udn membership label changes matter here.
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(
			r.enqueueAllRoutings,
		), builder.WithPredicates(namespaceMembershipChangePredicate())).
		Watches(&networkingapi.BGPRouting{}, handler.EnqueueRequestsFromMapFunc(
			r.enqueueAllRoutings,
		), builder.WithPredicates(predicate.Funcs{
			CreateFunc: func(event.CreateEvent) bool { return true },
			DeleteFunc: func(event.DeleteEvent) bool { return true },
			UpdateFunc: func(e event.UpdateEvent) bool {
				oldR, ok1 := e.ObjectOld.(*networkingapi.BGPRouting)
				newR, ok2 := e.ObjectNew.(*networkingapi.BGPRouting)
				if !ok1 || !ok2 {
					return true
				}
				return oldR.Spec.Network.Name != newR.Spec.Network.Name
			},
			GenericFunc: func(event.GenericEvent) bool { return false },
		})).
		Named("bgprouting").
		Complete(r)
}

// namespaceMembershipChangePredicate admits only the namespace events that can
// change a network's membership: a create or delete of a namespace carrying the
// cluster-udn label, or an update that adds, removes, or changes either
// membership label. Everything else (unrelated namespaces, annotation churn on
// a member) is filtered out so the routing controller is not woken for it.
func namespaceMembershipChangePredicate() predicate.Predicate {
	hasUDNLabel := func(o client.Object) bool {
		_, ok := o.GetLabels()[LabelClusterUDN]
		return ok
	}
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool { return hasUDNLabel(e.Object) },
		DeleteFunc: func(e event.DeleteEvent) bool { return hasUDNLabel(e.Object) },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldLabels, newLabels := e.ObjectOld.GetLabels(), e.ObjectNew.GetLabels()
			return labelChanged(oldLabels, newLabels, LabelClusterUDN) ||
				labelChanged(oldLabels, newLabels, LabelPrimaryUDN)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// labelChanged reports whether the given label differs between the two label
// maps, treating a missing label as distinct from a present-but-empty one so
// that adding or removing an empty-valued membership label is detected.
func labelChanged(oldLabels, newLabels map[string]string, key string) bool {
	oldVal, oldOK := oldLabels[key]
	newVal, newOK := newLabels[key]
	return oldOK != newOK || oldVal != newVal
}

// enqueueAllRoutings enqueues every BGPRouting so that a DuplicateNetwork
// conflict is re-evaluated across all CRs whenever any routing CR changes.
func (r *BGPRoutingReconciler) enqueueAllRoutings(ctx context.Context, _ client.Object) []reconcile.Request {
	list := &networkingapi.BGPRoutingList{}
	if err := r.List(ctx, list); err != nil {
		logf.FromContext(ctx).Error(err, "failed to list BGPRouting for enqueue-all")
		return nil
	}
	reqs := make([]reconcile.Request, len(list.Items))
	for i := range list.Items {
		reqs[i] = reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])}
	}
	return reqs
}

func (r *BGPRoutingReconciler) mapClusterUDNToRouting(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetLabels()[LabelManagedBy] != LabelManagedByVal {
		return nil
	}
	networkName := strings.TrimPrefix(obj.GetName(), ClusterUDNNamePrefix)
	routingList := &networkingapi.BGPRoutingList{}
	if err := r.List(ctx, routingList); err != nil {
		return nil
	}
	for _, rt := range routingList.Items {
		if rt.Spec.Network.Name == networkName {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: rt.Name}}}
		}
	}
	return nil
}
