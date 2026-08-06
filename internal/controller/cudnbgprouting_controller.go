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
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

// +kubebuilder:rbac:groups=networking.openshift.io,resources=cudnbgproutings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.openshift.io,resources=cudnbgproutings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.openshift.io,resources=cudnbgproutings/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=clusteruserdefinednetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=routeadvertisements,verbs=get;list;watch;create;update;patch;delete

type CUDNBgpRoutingReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *CUDNBgpRoutingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	routing := &networkingv1alpha1.CUDNBgpRouting{}
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

	routing.Status.Phase = networkingv1alpha1.PhaseConfiguring
	routing.Status.ObservedGeneration = routing.Generation

	// Pre-check: spec.network.name must be unique across all CUDNBgpRouting CRs
	routingList := &networkingv1alpha1.CUDNBgpRoutingList{}
	if err := r.List(ctx, routingList); err != nil {
		return ctrl.Result{}, err
	}
	for i := range routingList.Items {
		other := &routingList.Items[i]
		if other.Name != routing.Name && other.Spec.Network.Name == routing.Spec.Network.Name {
			return r.setDegraded(ctx, routing, *baselineStatus, networkingv1alpha1.ConditionCUDNCreated,
				"DuplicateNetwork",
				fmt.Sprintf("spec.network.name %q already claimed by CUDNBgpRouting %q", routing.Spec.Network.Name, other.Name))
		}
	}

	// Pre-check: CUDNBgpConfig must exist and be Ready
	bgpConfig := &networkingv1alpha1.CUDNBgpConfig{}
	if err := r.Get(ctx, types.NamespacedName{Name: SingletonName}, bgpConfig); err != nil {
		if err := r.patchRoutingStatus(ctx, routing, *baselineStatus, func(rt *networkingv1alpha1.CUDNBgpRouting) {
			rt.Status.Phase = networkingv1alpha1.PhasePending
			rt.Status.Conditions = nil
		}); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("CUDNBgpConfig 'cluster' not found, requeueing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if bgpConfig.Status.Phase != networkingv1alpha1.PhaseReady {
		if err := r.patchRoutingStatus(ctx, routing, *baselineStatus, func(rt *networkingv1alpha1.CUDNBgpRouting) {
			rt.Status.Phase = networkingv1alpha1.PhasePending
			rt.Status.Conditions = nil
		}); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("CUDNBgpConfig not Ready, requeueing", "phase", bgpConfig.Status.Phase)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Phase 1: Validate namespace labels + ensure CUDN
	log.Info("Phase 1: validating namespace and ensuring CUDN", "network", routing.Spec.Network.Name)
	if err := ValidateNamespaceLabels(ctx, r.Client, routing.Spec.Network.Name); err != nil {
		return r.setDegraded(ctx, routing, *baselineStatus, networkingv1alpha1.ConditionCUDNCreated,
			"NamespaceNotReady", fmt.Sprintf("namespace validation failed: %v", err))
	}
	if err := EnsureCUDN(ctx, r.Client, routing); err != nil {
		return r.setDegraded(ctx, routing, *baselineStatus, networkingv1alpha1.ConditionCUDNCreated,
			"CUDNFailed", fmt.Sprintf("failed to ensure CUDN: %v", err))
	}
	meta.SetStatusCondition(&routing.Status.Conditions, metav1.Condition{
		Type:               networkingv1alpha1.ConditionCUDNCreated,
		Status:             metav1.ConditionTrue,
		Reason:             "Created",
		Message:            fmt.Sprintf("CUDN %q ensured", CUDNNamePrefix+routing.Spec.Network.Name),
		ObservedGeneration: routing.Generation,
	})

	// Phase 2: Ensure shared RouteAdvertisements
	log.Info("Phase 2: ensuring RouteAdvertisements")
	if err := EnsureRouteAdvertisements(ctx, r.Client); err != nil {
		return r.setDegraded(ctx, routing, *baselineStatus, networkingv1alpha1.ConditionRouteAdvertisementsCreated,
			"RAFailed", fmt.Sprintf("failed to ensure RouteAdvertisements: %v", err))
	}
	meta.SetStatusCondition(&routing.Status.Conditions, metav1.Condition{
		Type:               networkingv1alpha1.ConditionRouteAdvertisementsCreated,
		Status:             metav1.ConditionTrue,
		Reason:             "Created",
		Message:            "Shared RouteAdvertisements ensured",
		ObservedGeneration: routing.Generation,
	})

	if err := r.patchRoutingStatus(ctx, routing, *baselineStatus, func(rt *networkingv1alpha1.CUDNBgpRouting) {
		rt.Status.Phase = networkingv1alpha1.PhaseReady
	}); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("reconciliation complete", "phase", routing.Status.Phase)
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

func (r *CUDNBgpRoutingReconciler) reconcileDelete(ctx context.Context, routing *networkingv1alpha1.CUDNBgpRouting) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Delete CUDN (namespace is left intact)
	log.Info("deleting CUDN", "network", routing.Spec.Network.Name)
	if err := DeleteCUDN(ctx, r.Client, routing.Spec.Network.Name); err != nil {
		return ctrl.Result{}, err
	}

	// Delete shared RouteAdvertisements only if this is the last CUDNBgpRouting
	routingList := &networkingv1alpha1.CUDNBgpRoutingList{}
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
		log.Info("last CUDNBgpRouting, deleting shared RouteAdvertisements")
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

func (r *CUDNBgpRoutingReconciler) setDegraded(
	ctx context.Context,
	routing *networkingv1alpha1.CUDNBgpRouting,
	baselineStatus networkingv1alpha1.CUDNBgpRoutingStatus,
	condType, reason, message string,
) (ctrl.Result, error) {
	logf.FromContext(ctx).Error(fmt.Errorf("%s: %s", reason, message), "setting degraded status")

	if err := r.patchRoutingStatus(ctx, routing, baselineStatus, func(rt *networkingv1alpha1.CUDNBgpRouting) {
		rt.Status.Phase = networkingv1alpha1.PhaseDegraded
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
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *CUDNBgpRoutingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cudn := &unstructured.Unstructured{}
	cudn.SetGroupVersionKind(CUDNNetworkGVK)

	ra := &unstructured.Unstructured{}
	ra.SetGroupVersionKind(RouteAdvertisementsGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha1.CUDNBgpRouting{}).
		Watches(cudn, handler.EnqueueRequestsFromMapFunc(
			r.mapCUDNToRouting,
		)).
		Watches(ra, handler.EnqueueRequestsFromMapFunc(
			r.mapRAToRouting,
		)).
		Named("cudnbgprouting").
		Complete(r)
}

func (r *CUDNBgpRoutingReconciler) mapCUDNToRouting(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetLabels()[LabelManagedBy] != LabelManagedByVal {
		return nil
	}
	networkName := strings.TrimPrefix(obj.GetName(), CUDNNamePrefix)
	routingList := &networkingv1alpha1.CUDNBgpRoutingList{}
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

func (r *CUDNBgpRoutingReconciler) mapRAToRouting(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetLabels()[LabelManagedBy] != LabelManagedByVal {
		return nil
	}
	routingList := &networkingv1alpha1.CUDNBgpRoutingList{}
	if err := r.List(ctx, routingList); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(routingList.Items))
	for _, rt := range routingList.Items {
		requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: rt.Name}})
	}
	return requests
}
