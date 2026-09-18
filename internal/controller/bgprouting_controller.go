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
// +kubebuilder:rbac:groups="",resources=nodes;pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=clusteruserdefinednetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=routeadvertisements,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch

type BGPRoutingReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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
			rt.Status.Phase = networkingapi.PhasePending
			rt.Status.Conditions = nil
		}); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("BGPCloudConfiguration 'cluster' not found, requeueing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if bgpConfig.Status.Phase != networkingapi.PhaseReady {
		if err := r.patchRoutingStatus(ctx, routing, *baselineStatus, func(rt *networkingapi.BGPRouting) {
			rt.Status.Phase = networkingapi.PhasePending
			rt.Status.Conditions = nil
		}); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("BGPCloudConfiguration not Ready, requeueing", "phase", bgpConfig.Status.Phase)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Phase 1: Validate namespace labels + ensure ClusterUDN
	log.Info("Phase 1: validating namespace and ensuring ClusterUDN", "network", routing.Spec.Network.Name)
	if err := ValidateNamespaceLabels(ctx, r.Client, routing.Spec.Network.Name); err != nil {
		return r.setDegraded(ctx, routing, *baselineStatus, networkingapi.ConditionNetworkCreated,
			ReasonNamespaceNotReady, fmt.Sprintf("namespace validation failed: %v", err))
	}
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
		Type:               networkingapi.ConditionNetworkCreated,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonCreated,
		Message:            fmt.Sprintf("ClusterUDN %q ensured", ClusterUDNNamePrefix+routing.Spec.Network.Name),
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

	// Phase 3: advertise host routes from each VM's hosting worker.
	log.Info("Phase 3: ensuring VM host routes")
	hostRouteCount, hostRoutesPending, err := EnsureVMHostRoutes(ctx, r.Client, routing, bgpConfig)
	if err != nil {
		return r.setDegraded(ctx, routing, *baselineStatus, networkingapi.ConditionVMHostRoutesConfigured,
			ReasonVMHostRoutesFailed, fmt.Sprintf("failed to ensure VM host routes: %v", err))
	}
	if hostRoutesPending {
		meta.SetStatusCondition(&routing.Status.Conditions, metav1.Condition{
			Type:               networkingapi.ConditionVMHostRoutesConfigured,
			Status:             metav1.ConditionUnknown,
			Reason:             ReasonWaitingForVMIPs,
			Message:            fmt.Sprintf("Configured %d VM host routes; waiting for VM addresses", hostRouteCount),
			ObservedGeneration: routing.Generation,
		})
		if err := r.patchRoutingStatus(ctx, routing, *baselineStatus, func(rt *networkingapi.BGPRouting) {
			rt.Status.Phase = networkingapi.PhaseConfiguring
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	meta.SetStatusCondition(&routing.Status.Conditions, metav1.Condition{
		Type:               networkingapi.ConditionVMHostRoutesConfigured,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonReconciled,
		Message:            fmt.Sprintf("Configured %d VM host routes", hostRouteCount),
		ObservedGeneration: routing.Generation,
	})

	if err := r.patchRoutingStatus(ctx, routing, *baselineStatus, func(rt *networkingapi.BGPRouting) {
		rt.Status.Phase = networkingapi.PhaseReady
	}); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("reconciliation complete", "phase", routing.Status.Phase)
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *BGPRoutingReconciler) reconcileDelete(ctx context.Context, routing *networkingapi.BGPRouting) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if err := DeleteVMHostRoutes(ctx, r.Client, routing.Name); err != nil {
		return ctrl.Result{}, err
	}

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
		rt.Status.Phase = networkingapi.PhaseDegraded
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
	if terminal {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// SetupWithManager conditionally watches VMIs when KubeVirt is already
// installed. The virt-launcher Pod watch provides lifecycle events when the
// optional KubeVirt API is unavailable at operator startup.
func (r *BGPRoutingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cudn := &unstructured.Unstructured{}
	cudn.SetGroupVersionKind(ClusterUDNGVK)

	controllerBuilder := ctrl.NewControllerManagedBy(mgr).
		For(&networkingapi.BGPRouting{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(
			r.mapWorkloadToRouting,
		), builder.WithPredicates(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool { return isVirtLauncher(e.Object) },
			DeleteFunc: func(e event.DeleteEvent) bool { return isVirtLauncher(e.Object) },
			UpdateFunc: func(e event.UpdateEvent) bool {
				oldPod, oldOK := e.ObjectOld.(*corev1.Pod)
				newPod, newOK := e.ObjectNew.(*corev1.Pod)
				if !oldOK || !newOK || (!isVirtLauncher(oldPod) && !isVirtLauncher(newPod)) {
					return false
				}
				return oldPod.Spec.NodeName != newPod.Spec.NodeName ||
					oldPod.Status.Phase != newPod.Status.Phase ||
					oldPod.DeletionTimestamp.IsZero() != newPod.DeletionTimestamp.IsZero()
			},
			GenericFunc: func(event.GenericEvent) bool { return false },
		})).
		Watches(cudn, handler.EnqueueRequestsFromMapFunc(
			r.mapClusterUDNToRouting,
		)).
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
		}))

	if _, err := mgr.GetRESTMapper().RESTMapping(VirtualMachineInstanceGVK.GroupKind(), VirtualMachineInstanceGVK.Version); err == nil {
		vmi := &unstructured.Unstructured{}
		vmi.SetGroupVersionKind(VirtualMachineInstanceGVK)
		controllerBuilder = controllerBuilder.Watches(vmi, handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToRouting))
	} else if !meta.IsNoMatchError(err) {
		return fmt.Errorf("discovering VirtualMachineInstance API: %w", err)
	}

	return controllerBuilder.Named("bgprouting").Complete(r)
}

func isVirtLauncher(obj client.Object) bool {
	return obj.GetLabels()["kubevirt.io"] == "virt-launcher"
}

func (r *BGPRoutingReconciler) mapWorkloadToRouting(ctx context.Context, obj client.Object) []reconcile.Request {
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: obj.GetNamespace()}, namespace); err != nil {
		return nil
	}
	networkName := namespace.Labels[LabelClusterUDN]
	primaryNetwork, hasPrimaryNetwork := namespace.Labels[LabelPrimaryUDN]
	if networkName == "" || !hasPrimaryNetwork || primaryNetwork != "" {
		return nil
	}

	routingList := &networkingapi.BGPRoutingList{}
	if err := r.List(ctx, routingList); err != nil {
		logf.FromContext(ctx).Error(err, "failed to list BGPRouting for workload event")
		return nil
	}
	requests := make([]reconcile.Request, 0, 1)
	for i := range routingList.Items {
		if routingList.Items[i].Spec.Network.Name == networkName {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&routingList.Items[i])})
		}
	}
	return requests
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
