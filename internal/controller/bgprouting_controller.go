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
	"reflect"
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
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
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

	// Namespace selection is authoritative for route ownership. When no selected
	// namespace remains, withdraw all routes even if other prerequisites are not
	// ready. Preserve routes when the namespace API result is inconclusive.
	namespaceErr := ValidateNamespaceLabels(ctx, r.Client, routing.Spec.Network.Name)
	noMatchingNamespaces := errors.Is(namespaceErr, errNoMatchingNamespaces)
	if noMatchingNamespaces {
		if err := DeleteVMHostRoutes(ctx, r.Client, routing.Name); err != nil {
			return ctrl.Result{}, fmt.Errorf("removing VM host routes after the last matching namespace disappeared: %w", err)
		}
		meta.SetStatusCondition(&routing.Status.Conditions, metav1.Condition{
			Type:               networkingapi.ConditionVMHostRoutesConfigured,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonReconciled,
			Message:            "Configured 0 VM host routes",
			ObservedGeneration: routing.Generation,
		})
	}

	// Pre-check: spec.network.name must be unique across all BGPRouting CRs
	routingList := &networkingapi.BGPRoutingList{}
	if err := r.List(ctx, routingList); err != nil {
		return ctrl.Result{}, err
	}
	for i := range routingList.Items {
		other := &routingList.Items[i]
		if other.Name != routing.Name && other.Spec.Network.Name == routing.Spec.Network.Name {
			if err := DeleteVMHostRoutes(ctx, r.Client, routing.Name); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing VM host routes after detecting duplicate network %q: %w",
					routing.Spec.Network.Name, err)
			}
			meta.SetStatusCondition(&routing.Status.Conditions, metav1.Condition{
				Type:               networkingapi.ConditionVMHostRoutesConfigured,
				Status:             metav1.ConditionTrue,
				Reason:             ReasonReconciled,
				Message:            "Configured 0 VM host routes",
				ObservedGeneration: routing.Generation,
			})
			return r.setDegraded(ctx, routing, *baselineStatus, networkingapi.ConditionNetworkCreated,
				ReasonDuplicateNetwork,
				fmt.Sprintf("spec.network.name %q already claimed by BGPRouting %q", routing.Spec.Network.Name, other.Name))
		}
	}
	if noMatchingNamespaces {
		return r.setDegraded(ctx, routing, *baselineStatus, networkingapi.ConditionNetworkCreated,
			ReasonNamespaceNotReady, fmt.Sprintf("namespace validation failed: %v", namespaceErr))
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
	if namespaceErr != nil {
		return r.setDegraded(ctx, routing, *baselineStatus, networkingapi.ConditionNetworkCreated,
			ReasonNamespaceNotReady, fmt.Sprintf("namespace validation failed: %v", namespaceErr))
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
	hostRoutes, err := EnsureVMHostRoutes(ctx, r.Client, routing, bgpConfig)
	if err != nil {
		if errors.Is(err, errVMIAPIUnavailable) {
			meta.SetStatusCondition(&routing.Status.Conditions, metav1.Condition{
				Type:               networkingapi.ConditionVMHostRoutesConfigured,
				Status:             metav1.ConditionFalse,
				Reason:             ReasonVMIAPIUnavailable,
				Message:            "VirtualMachineInstance API is unavailable; existing VM routes are preserved. Restore KubeVirt or remove stale VM host-route FRRConfigurations after confirming the VMs are gone",
				ObservedGeneration: routing.Generation,
			})
			if patchErr := r.patchRoutingStatus(ctx, routing, *baselineStatus, func(rt *networkingapi.BGPRouting) {
				rt.Status.Phase = networkingapi.PhaseReady
			}); patchErr != nil {
				return ctrl.Result{}, patchErr
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		return r.setDegraded(ctx, routing, *baselineStatus, networkingapi.ConditionVMHostRoutesConfigured,
			ReasonVMHostRoutesFailed, fmt.Sprintf("failed to ensure VM host routes: %v", err))
	}
	// Phase tracks the network. Neither a VM waiting for an address nor one
	// that cannot be given a host route at all makes the ClusterUDN and the
	// RouteAdvertisements less configured, and either would otherwise hold the
	// CR short of Ready for as long as that VM exists. The condition carries
	// which of the two it is.
	switch {
	case len(hostRoutes.Unserved) > 0:
		meta.SetStatusCondition(&routing.Status.Conditions, metav1.Condition{
			Type:   networkingapi.ConditionVMHostRoutesConfigured,
			Status: metav1.ConditionFalse,
			Reason: ReasonVMHostRoutesIncomplete,
			Message: truncateConditionMessage(fmt.Sprintf("Configured %d VM host routes; %s",
				hostRoutes.Configured, strings.Join(hostRoutes.Unserved, "; "))),
			ObservedGeneration: routing.Generation,
		})
	case hostRoutes.Pending:
		meta.SetStatusCondition(&routing.Status.Conditions, metav1.Condition{
			Type:               networkingapi.ConditionVMHostRoutesConfigured,
			Status:             metav1.ConditionUnknown,
			Reason:             ReasonWaitingForVMIPs,
			Message:            fmt.Sprintf("Configured %d VM host routes; waiting for VM addresses", hostRoutes.Configured),
			ObservedGeneration: routing.Generation,
		})
	default:
		meta.SetStatusCondition(&routing.Status.Conditions, metav1.Condition{
			Type:               networkingapi.ConditionVMHostRoutesConfigured,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonReconciled,
			Message:            fmt.Sprintf("Configured %d VM host routes", hostRoutes.Configured),
			ObservedGeneration: routing.Generation,
		})
	}

	if err := r.patchRoutingStatus(ctx, routing, *baselineStatus, func(rt *networkingapi.BGPRouting) {
		rt.Status.Phase = networkingapi.PhaseReady
	}); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("reconciliation complete", "phase", routing.Status.Phase)
	if hostRoutes.Pending {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
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

// SetupWithManager watches VMIs when their API is available during startup.
// Otherwise, virt-launcher Pod events trigger the same reconciliation.
// CacheOptions limits that Pod informer at the API server.
func (r *BGPRoutingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	cudn := &unstructured.Unstructured{}
	cudn.SetGroupVersionKind(ClusterUDNGVK)

	controllerBuilder := ctrl.NewControllerManagedBy(mgr).
		For(&networkingapi.BGPRouting{}).
		Watches(cudn, handler.EnqueueRequestsFromMapFunc(
			r.mapClusterUDNToRouting,
		)).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(
			r.enqueueAllRoutings,
		), builder.WithPredicates(nodeLabelChangePredicate())).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(
			r.mapNamespaceToRouting,
		), builder.WithPredicates(namespaceLabelChangePredicate())).
		Watches(&networkingapi.BGPCloudConfiguration{}, handler.EnqueueRequestsFromMapFunc(
			r.enqueueAllRoutings,
		), builder.WithPredicates(configRelevantToRoutingPredicate())).
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

	workload, err := workloadWatchObject(mgr.GetRESTMapper())
	if err != nil {
		return err
	}
	if _, isPod := workload.(*corev1.Pod); isPod {
		controllerBuilder = controllerBuilder.Watches(workload, handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToRouting))
	} else {
		controllerBuilder = controllerBuilder.Watches(workload, handler.EnqueueRequestsFromMapFunc(r.mapWorkloadToRouting),
			builder.WithPredicates(vmiRouteChangePredicate()))
	}

	return controllerBuilder.Named("bgprouting").Complete(r)
}

func workloadWatchObject(mapper meta.RESTMapper) (client.Object, error) {
	if _, err := mapper.RESTMapping(VirtualMachineInstanceGVK.GroupKind(), VirtualMachineInstanceGVK.Version); err == nil {
		vmi := &unstructured.Unstructured{}
		vmi.SetGroupVersionKind(VirtualMachineInstanceGVK)
		return vmi, nil
	} else if !meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("discovering VirtualMachineInstance API: %w", err)
	}
	return &corev1.Pod{}, nil
}

func (r *BGPRoutingReconciler) mapWorkloadToRouting(ctx context.Context, obj client.Object) []reconcile.Request {
	namespace := &corev1.Namespace{}
	if err := r.Get(ctx, types.NamespacedName{Name: obj.GetNamespace()}, namespace); err != nil {
		return nil
	}
	return r.mapNamespaceToRouting(ctx, namespace)
}

// The event object's labels identify the network affected by a namespace
// deletion or either side of a namespace label change.
func (r *BGPRoutingReconciler) mapNamespaceToRouting(ctx context.Context, obj client.Object) []reconcile.Request {
	networkName := obj.GetLabels()[LabelClusterUDN]
	primaryNetwork, hasPrimaryNetwork := obj.GetLabels()[LabelPrimaryUDN]
	if networkName == "" || !hasPrimaryNetwork || primaryNetwork != "" {
		return nil
	}

	routingList := &networkingapi.BGPRoutingList{}
	if err := r.List(ctx, routingList); err != nil {
		logf.FromContext(ctx).Error(err, "failed to list BGPRouting for namespace or workload event")
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

func namespaceLabelChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNamespace, ok1 := e.ObjectOld.(*corev1.Namespace)
			newNamespace, ok2 := e.ObjectNew.(*corev1.Namespace)
			if !ok1 || !ok2 {
				return true
			}
			oldPrimary, oldHasPrimary := oldNamespace.Labels[LabelPrimaryUDN]
			newPrimary, newHasPrimary := newNamespace.Labels[LabelPrimaryUDN]
			return oldNamespace.Labels[LabelClusterUDN] != newNamespace.Labels[LabelClusterUDN] ||
				oldPrimary != newPrimary || oldHasPrimary != newHasPrimary
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
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

// nodeLabelChangePredicate passes only the node changes that alter what this
// controller writes. Labels decide whether a node is in the router pool and
// which peer group it belongs to; nothing else about a node is read, and node
// status churns on every kubelet heartbeat.
func nodeLabelChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, ok1 := e.ObjectOld.(*corev1.Node)
			newNode, ok2 := e.ObjectNew.(*corev1.Node)
			if !ok1 || !ok2 {
				return true
			}
			return !reflect.DeepEqual(oldNode.Labels, newNode.Labels)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// vmiRouteChangePredicate passes changes that can alter a host route: deletion,
// phase, hosting node, or guest interfaces.
func vmiRouteChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldVMI, ok1 := e.ObjectOld.(*unstructured.Unstructured)
			newVMI, ok2 := e.ObjectNew.(*unstructured.Unstructured)
			if !ok1 || !ok2 {
				return true
			}
			if !reflect.DeepEqual(oldVMI.GetDeletionTimestamp(), newVMI.GetDeletionTimestamp()) {
				return true
			}
			for _, field := range []string{"phase", "nodeName", "interfaces"} {
				oldValue, _, _ := unstructured.NestedFieldNoCopy(oldVMI.Object, "status", field)
				newValue, _, _ := unstructured.NestedFieldNoCopy(newVMI.Object, "status", field)
				if !reflect.DeepEqual(oldValue, newValue) {
					return true
				}
			}
			return false
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// configRelevantToRoutingPredicate passes the BGPCloudConfiguration changes
// that alter the VM host routes: the spec, which carries routerNodeSelector and
// the BGP settings, the phase, which gates reconciling at all, and the peer
// groups a cloud discovers, which supply the neighbour list. Its conditions
// churn without changing any of that.
func configRelevantToRoutingPredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldConfig, ok1 := e.ObjectOld.(*networkingapi.BGPCloudConfiguration)
			newConfig, ok2 := e.ObjectNew.(*networkingapi.BGPCloudConfiguration)
			if !ok1 || !ok2 {
				return true
			}
			return oldConfig.Generation != newConfig.Generation ||
				oldConfig.Status.Phase != newConfig.Status.Phase ||
				!reflect.DeepEqual(oldConfig.Status.PeerGroups, newConfig.Status.PeerGroups)
		},
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
