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
	"time"

	"reflect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
	awsplatform "github.com/openshift/bgp-cloud-connector/internal/platform/aws"
)

// +kubebuilder:rbac:groups=networking.openshift.io,resources=cudnbgpconfigs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.openshift.io,resources=cudnbgpconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.openshift.io,resources=cudnbgpconfigs/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.openshift.io,resources=cudnbgproutings,verbs=get;list;watch
// +kubebuilder:rbac:groups=operator.openshift.io,resources=networks,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=frrk8s.metallb.io,resources=frrconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=infrastructures,verbs=get
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=create;delete;update;patch

type PlatformBuilderFunc func(ctx context.Context, c client.Client, config *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error)

type CUDNBgpConfigReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	PlatformBuilder PlatformBuilderFunc
}

func (r *CUDNBgpConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	config := &networkingv1alpha1.CUDNBgpConfig{}
	if err := r.Get(ctx, req.NamespacedName, config); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	baselineStatus := config.Status.DeepCopy()

	if config.Name != SingletonName {
		return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionNetworkOperatorPatched,
			ReasonInvalidName, fmt.Sprintf("CUDNBgpConfig must be named %q, got %q", SingletonName, config.Name))
	}

	if !config.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, config)
	}

	if !controllerutil.ContainsFinalizer(config, ConfigFinalizerName) {
		controllerutil.AddFinalizer(config, ConfigFinalizerName)
		if err := r.Update(ctx, config); err != nil {
			return ctrl.Result{}, err
		}
	}

	config.Status.Phase = networkingv1alpha1.PhaseConfiguring
	config.Status.ObservedGeneration = config.Generation

	// Phase 1: Patch Network Operator
	log.Info("Phase 1: patching Network operator")
	if err := PatchNetworkOperator(ctx, r.Client); err != nil {
		return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionNetworkOperatorPatched,
			ReasonPatchFailed, fmt.Sprintf("failed to patch Network operator: %v", err))
	}
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:               networkingv1alpha1.ConditionNetworkOperatorPatched,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonPatched,
		Message:            "Network operator patched with FRR and routeAdvertisements",
		ObservedGeneration: config.Generation,
	})

	// Phase 2: Wait for FRR
	log.Info("Phase 2: checking FRR readiness")
	ready, err := IsFRRReady(ctx, r.Client)
	if err != nil {
		return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionFRRNamespaceReady,
			ReasonCheckFailed, fmt.Sprintf("failed to check FRR readiness: %v", err))
	}
	if !ready {
		if err := r.patchConfigStatus(ctx, config, *baselineStatus, func(c *networkingv1alpha1.CUDNBgpConfig) {
			c.Status.Phase = networkingv1alpha1.PhaseConfiguring
			c.Status.ObservedGeneration = c.Generation
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type:               networkingv1alpha1.ConditionFRRNamespaceReady,
				Status:             metav1.ConditionFalse,
				Reason:             ReasonWaitingForFRR,
				Message:            "Waiting for openshift-frr-k8s namespace and pods",
				ObservedGeneration: c.Generation,
			})
		}); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("FRR not ready, requeueing")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:               networkingv1alpha1.ConditionFRRNamespaceReady,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonFRRReady,
		Message:            "FRR namespace and pods are running",
		ObservedGeneration: config.Generation,
	})

	// Build the cloud platform once if configured (used in Phases 3 and 5)
	var cloudPlatform platform.CloudPlatform
	var discoveryResult *platform.DiscoveryResult
	if config.Spec.Platform != networkingv1alpha1.PlatformManual {
		buildPlatform := r.PlatformBuilder
		if buildPlatform == nil {
			buildPlatform = defaultPlatformBuilder
		}
		p, err := buildPlatform(ctx, r.Client, config)
		if err != nil {
			var credErr *platform.CredentialError
			if errors.As(err, &credErr) {
				return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionCloudEndpointsDiscovered,
					ReasonCloudCredentialsInvalid, credErr.Error())
			}
			return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionCloudEndpointsDiscovered,
				ReasonCloudDiscoveryFailed, fmt.Sprintf("failed to build the cloud platform: %v", err))
		}
		cloudPlatform = p

		// Phase 3: Discover the cloud's BGP endpoints
		log.Info("Phase 3: discovering cloud BGP endpoints")
		discoveryResult, err = cloudPlatform.DiscoverEndpoints(ctx)
		if err != nil {
			var notFoundErr *awsplatform.RouteServerNotFoundError
			if errors.As(err, &notFoundErr) {
				return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionCloudEndpointsDiscovered,
					ReasonRouteServerNotFound, notFoundErr.Error())
			}
			return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionCloudEndpointsDiscovered,
				ReasonCloudDiscoveryFailed, fmt.Sprintf("failed to discover cloud BGP endpoints: %v", err))
		}
		config.Status.PeerGroups = peerGroupsToStatus(discoveryResult.PeerGroups)
		meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
			Type:               networkingv1alpha1.ConditionCloudEndpointsDiscovered,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonDiscovered,
			Message:            fmt.Sprintf("Discovered %d peer group(s)", len(discoveryResult.PeerGroups)),
			ObservedGeneration: config.Generation,
		})
	}

	// Phase 4: Apply FRR Configuration per peer group
	log.Info("Phase 4: applying FRR configurations")
	var frrCount int
	if discoveryResult != nil {
		frrCount, err = EnsureFRRConfigurationsFromGroups(ctx, r.Client, config, discoveryResult.PeerGroups)
	} else {
		frrCount = len(config.Spec.BGP.PeerGroups)
		err = EnsureFRRConfigurations(ctx, r.Client, config)
	}
	if err != nil {
		return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionFRRConfigurationApplied,
			ReasonApplyFailed, fmt.Sprintf("failed to apply FRR configurations: %v", err))
	}
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:               networkingv1alpha1.ConditionFRRConfigurationApplied,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonApplied,
		Message:            fmt.Sprintf("Applied %d FRRConfiguration(s)", frrCount),
		ObservedGeneration: config.Generation,
	})

	// Phase 5: Reconcile cloud resources (if configured)
	if cloudPlatform != nil {
		log.Info("Phase 5: reconciling cloud resources")
		nodes, err := r.listRouterNodes(ctx, config)
		if err != nil {
			return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionCloudResourcesReconciled,
				ReasonCloudReconcileFailed, fmt.Sprintf("failed to list router nodes: %v", err))
		}
		if err := cloudPlatform.ReconcileNodes(ctx, nodes); err != nil {
			return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionCloudResourcesReconciled,
				ReasonCloudReconcileFailed, fmt.Sprintf("failed to reconcile cloud resources: %v", err))
		}
		meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
			Type:               networkingv1alpha1.ConditionCloudResourcesReconciled,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonReconciled,
			Message:            "Cloud BGP peerings and router node settings reconciled",
			ObservedGeneration: config.Generation,
		})
	}

	if err := r.patchConfigStatus(ctx, config, *baselineStatus, func(c *networkingv1alpha1.CUDNBgpConfig) {
		c.Status.Phase = networkingv1alpha1.PhaseReady
	}); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("reconciliation complete", "phase", config.Status.Phase)
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// peerGroupsToStatus reports the peering plan, which is what FRR was told to
// peer with and therefore the thing worth reading.
func peerGroupsToStatus(groups []platform.PeerGroup) []networkingv1alpha1.PeerGroupStatus {
	if len(groups) == 0 {
		return nil
	}
	out := make([]networkingv1alpha1.PeerGroupStatus, 0, len(groups))
	for _, g := range groups {
		pg := networkingv1alpha1.PeerGroupStatus{
			Key:          g.Key,
			NodeSelector: g.NodeSelector,
		}
		for _, n := range g.Neighbors {
			pg.Neighbors = append(pg.Neighbors, networkingv1alpha1.BGPNeighbor{
				Address:   n.Address,
				RemoteASN: n.ASN,
			})
		}
		out = append(out, pg)
	}
	return out
}

// defaultPlatformBuilder constructs the cloud platform named by spec.platform.
// PlatformManual never reaches here: the controller skips platform
// construction entirely for it.
func defaultPlatformBuilder(ctx context.Context, c client.Client, config *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
	switch config.Spec.Platform {
	case networkingv1alpha1.PlatformAWS:
		return buildAWSPlatform(ctx, c, config)
	default:
		return nil, fmt.Errorf("no platform implementation for %q", config.Spec.Platform)
	}
}

func buildAWSPlatform(ctx context.Context, c client.Client, config *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
	awsSpec := config.Spec.AWS

	clusterID, err := getInfrastructureName(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("reading cluster infrastructure name: %w", err)
	}

	cfg := awsplatform.Config{
		Region:            awsSpec.Region,
		RouteServerIDs:    awsSpec.RouteServerIDs,
		LocalASN:          config.Spec.BGP.LocalASN,
		LivenessDetection: string(config.Spec.BGP.LivenessDetection),
		ClusterID:         clusterID,
	}

	return awsplatform.New(ctx, cfg)
}

func getInfrastructureName(ctx context.Context, c client.Client) (string, error) {
	infra := &unstructured.Unstructured{}
	infra.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "config.openshift.io",
		Version: "v1",
		Kind:    "Infrastructure",
	})
	if err := c.Get(ctx, types.NamespacedName{Name: "cluster"}, infra); err != nil {
		return "", err
	}
	name, found, err := unstructured.NestedString(infra.Object, "status", "infrastructureName")
	if err != nil || !found || name == "" {
		return "", fmt.Errorf("status.infrastructureName not set on Infrastructure/cluster")
	}
	return name, nil
}

func (r *CUDNBgpConfigReconciler) listRouterNodes(ctx context.Context, config *networkingv1alpha1.CUDNBgpConfig) ([]platform.RouterNode, error) {
	nodeList := &corev1.NodeList{}
	sel := labels.SelectorFromSet(config.Spec.RouterNodeSelector)
	if err := r.List(ctx, nodeList, client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}

	nodes := make([]platform.RouterNode, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		rn := platform.RouterNode{
			Name:       node.Name,
			ProviderID: node.Spec.ProviderID,
			Zone:       node.Labels["topology.kubernetes.io/zone"],
		}
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				rn.PrivateIP = addr.Address
				break
			}
		}
		if rn.PrivateIP == "" || rn.Zone == "" || rn.ProviderID == "" {
			logf.FromContext(ctx).Info("skipping node with incomplete info",
				"node", node.Name, "ip", rn.PrivateIP, "zone", rn.Zone, "providerID", rn.ProviderID)
			continue
		}
		nodes = append(nodes, rn)
	}
	return nodes, nil
}

func (r *CUDNBgpConfigReconciler) reconcileDelete(ctx context.Context, config *networkingv1alpha1.CUDNBgpConfig) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	baselineStatus := config.Status.DeepCopy()

	routingList := &networkingv1alpha1.CUDNBgpRoutingList{}
	if err := r.List(ctx, routingList); err != nil {
		return ctrl.Result{}, err
	}
	if len(routingList.Items) > 0 {
		log.Info("deletion blocked: CUDNBgpRouting CRs still exist", "count", len(routingList.Items))
		if err := r.reportDeletionBlocked(ctx, config, *baselineStatus, routingList.Items); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if config.Spec.Platform != networkingv1alpha1.PlatformManual {
		log.Info("cleaning up cloud resources")
		buildPlatform := r.PlatformBuilder
		if buildPlatform == nil {
			buildPlatform = defaultPlatformBuilder
		}
		p, err := buildPlatform(ctx, r.Client, config)
		if err != nil {
			log.Error(err, "unable to build the cloud platform for cleanup, skipping cloud resource cleanup")
		} else if _, err := p.DiscoverEndpoints(ctx); err != nil {
			log.Error(err, "unable to discover endpoints for cleanup, skipping cloud resource cleanup")
		} else if err := p.Cleanup(ctx); err != nil {
			return ctrl.Result{}, fmt.Errorf("cleaning up cloud resources: %w", err)
		}
	}

	log.Info("cleaning up FRR configurations")
	if err := DeleteFRRConfigurations(ctx, r.Client); err != nil {
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(config, ConfigFinalizerName)
	if err := r.Update(ctx, config); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("finalizer removed, deletion complete")
	return ctrl.Result{}, nil
}

func (r *CUDNBgpConfigReconciler) setDegraded(
	ctx context.Context,
	config *networkingv1alpha1.CUDNBgpConfig,
	baselineStatus networkingv1alpha1.CUDNBgpConfigStatus,
	condType, reason, message string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if _, terminal := TerminalDegradedReasons[reason]; terminal {
		log.Info("terminal degraded condition, not requeueing", "reason", reason, "message", message)
	} else {
		log.Error(fmt.Errorf("%s: %s", reason, message), "setting degraded status")
	}

	if err := r.patchConfigStatus(ctx, config, baselineStatus, func(c *networkingv1alpha1.CUDNBgpConfig) {
		c.Status.Phase = networkingv1alpha1.PhaseDegraded
		meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: c.Generation,
		})
	}); err != nil {
		return ctrl.Result{}, err
	}
	if _, terminal := TerminalDegradedReasons[reason]; terminal {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func nodeRelevantChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldNode, ok1 := e.ObjectOld.(*corev1.Node)
			newNode, ok2 := e.ObjectNew.(*corev1.Node)
			if !ok1 || !ok2 {
				return true
			}
			if !reflect.DeepEqual(oldNode.Labels, newNode.Labels) {
				return true
			}
			if oldNode.Spec.ProviderID != newNode.Spec.ProviderID {
				return true
			}
			return !reflect.DeepEqual(oldNode.Status.Addresses, newNode.Status.Addresses)
		},
	}
}

func (r *CUDNBgpConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	frrCfg := &unstructured.Unstructured{}
	frrCfg.SetGroupVersionKind(FRRConfigurationGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1alpha1.CUDNBgpConfig{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: SingletonName}}}
			},
		), builder.WithPredicates(nodeRelevantChangePredicate())).
		Watches(frrCfg, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, obj client.Object) []reconcile.Request {
				if obj.GetLabels()[LabelManagedBy] != LabelManagedByVal {
					return nil
				}
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: SingletonName}}}
			},
		)).
		Watches(&networkingv1alpha1.CUDNBgpRouting{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: SingletonName}}}
			},
		), builder.WithPredicates(predicate.Funcs{
			CreateFunc:  func(event.CreateEvent) bool { return true },
			DeleteFunc:  func(event.DeleteEvent) bool { return true },
			UpdateFunc:  func(event.UpdateEvent) bool { return false },
			GenericFunc: func(event.GenericEvent) bool { return false },
		})).
		Named("cudnbgpconfig").
		Complete(r)
}
