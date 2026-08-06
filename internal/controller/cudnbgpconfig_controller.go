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
			"InvalidName", fmt.Sprintf("CUDNBgpConfig must be named %q, got %q", SingletonName, config.Name))
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
			"PatchFailed", fmt.Sprintf("failed to patch Network operator: %v", err))
	}
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:               networkingv1alpha1.ConditionNetworkOperatorPatched,
		Status:             metav1.ConditionTrue,
		Reason:             "Patched",
		Message:            "Network operator patched with FRR and routeAdvertisements",
		ObservedGeneration: config.Generation,
	})

	// Phase 2: Wait for FRR
	log.Info("Phase 2: checking FRR readiness")
	ready, err := IsFRRReady(ctx, r.Client)
	if err != nil {
		return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionFRRNamespaceReady,
			"CheckFailed", fmt.Sprintf("failed to check FRR readiness: %v", err))
	}
	if !ready {
		if err := r.patchConfigStatus(ctx, config, *baselineStatus, func(c *networkingv1alpha1.CUDNBgpConfig) {
			c.Status.Phase = networkingv1alpha1.PhaseConfiguring
			c.Status.ObservedGeneration = c.Generation
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type:               networkingv1alpha1.ConditionFRRNamespaceReady,
				Status:             metav1.ConditionFalse,
				Reason:             "WaitingForFRR",
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
		Reason:             "Ready",
		Message:            "FRR namespace and pods are running",
		ObservedGeneration: config.Generation,
	})

	// Build AWS platform once if configured (used in Phases 3 and 5)
	var awsPlatform platform.CloudPlatform
	var discoveryResult *platform.DiscoveryResult
	if config.Spec.AWS != nil {
		buildPlatform := r.PlatformBuilder
		if buildPlatform == nil {
			buildPlatform = defaultPlatformBuilder
		}
		p, err := buildPlatform(ctx, r.Client, config)
		if err != nil {
			var credErr *awsplatform.CredentialError
			if errors.As(err, &credErr) {
				return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionAWSEndpointsDiscovered,
					"AWSCredentialsInvalid", credErr.Error())
			}
			return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionAWSEndpointsDiscovered,
				"AWSDiscoveryFailed", fmt.Sprintf("failed to build AWS platform: %v", err))
		}
		awsPlatform = p

		// Phase 3: Discover Route Server Infrastructure
		log.Info("Phase 3: discovering Route Server endpoints")
		discoveryResult, err = awsPlatform.DiscoverEndpoints(ctx)
		if err != nil {
			return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionAWSEndpointsDiscovered,
				"AWSDiscoveryFailed", fmt.Sprintf("failed to discover Route Server endpoints: %v", err))
		}
		config.Status.AWS = discoveryResultToStatus(discoveryResult)
		meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
			Type:               networkingv1alpha1.ConditionAWSEndpointsDiscovered,
			Status:             metav1.ConditionTrue,
			Reason:             "Discovered",
			Message:            fmt.Sprintf("Discovered %d Route Server(s) with endpoints across %d AZ(s)", len(discoveryResult.RouteServers), len(discoveryResult.NeighborsByAZ)),
			ObservedGeneration: config.Generation,
		})
	}

	// Phase 4: Apply FRR Configuration per AZ
	log.Info("Phase 4: applying FRR configurations")
	var frrCount int
	if config.Spec.AWS != nil && discoveryResult != nil {
		frrCount, err = EnsureFRRConfigurationsFromDiscovery(ctx, r.Client, config, discoveryResult)
	} else {
		frrCount = len(config.Spec.BGP.AvailabilityZones)
		err = EnsureFRRConfigurations(ctx, r.Client, config)
	}
	if err != nil {
		return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionFRRConfigurationApplied,
			"ApplyFailed", fmt.Sprintf("failed to apply FRR configurations: %v", err))
	}
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:               networkingv1alpha1.ConditionFRRConfigurationApplied,
		Status:             metav1.ConditionTrue,
		Reason:             "Applied",
		Message:            fmt.Sprintf("Applied %d FRRConfiguration(s)", frrCount),
		ObservedGeneration: config.Generation,
	})

	// Phase 5: Reconcile AWS Resources (if configured)
	if awsPlatform != nil {
		log.Info("Phase 5: reconciling AWS resources")
		nodes, err := r.listRouterNodes(ctx, config)
		if err != nil {
			return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionAWSResourcesReconciled,
				"AWSReconcileFailed", fmt.Sprintf("failed to list router nodes: %v", err))
		}
		if err := awsPlatform.ReconcileNodes(ctx, nodes); err != nil {
			return r.setDegraded(ctx, config, *baselineStatus, networkingv1alpha1.ConditionAWSResourcesReconciled,
				"AWSReconcileFailed", fmt.Sprintf("failed to reconcile AWS resources: %v", err))
		}
		meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
			Type:               networkingv1alpha1.ConditionAWSResourcesReconciled,
			Status:             metav1.ConditionTrue,
			Reason:             "Reconciled",
			Message:            "Route Server peers and source/dest check reconciled",
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

func discoveryResultToStatus(dr *platform.DiscoveryResult) *networkingv1alpha1.AWSStatus {
	status := &networkingv1alpha1.AWSStatus{}
	for _, rs := range dr.RouteServers {
		drs := networkingv1alpha1.DiscoveredRouteServer{
			RouteServerID: rs.RouteServerID,
			RemoteASN:     rs.RemoteASN,
		}
		for _, ep := range rs.Endpoints {
			drs.Endpoints = append(drs.Endpoints, networkingv1alpha1.DiscoveredEndpoint{
				EndpointID:       ep.EndpointID,
				AvailabilityZone: ep.AZ,
				Address:          ep.Address,
			})
		}
		status.RouteServers = append(status.RouteServers, drs)
	}
	return status
}

func defaultPlatformBuilder(ctx context.Context, c client.Client, config *networkingv1alpha1.CUDNBgpConfig) (platform.CloudPlatform, error) {
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
			AZ:         node.Labels["topology.kubernetes.io/zone"],
		}
		for _, addr := range node.Status.Addresses {
			if addr.Type == corev1.NodeInternalIP {
				rn.PrivateIP = addr.Address
				break
			}
		}
		if rn.PrivateIP == "" || rn.AZ == "" || rn.ProviderID == "" {
			logf.FromContext(ctx).Info("skipping node with incomplete info",
				"node", node.Name, "ip", rn.PrivateIP, "az", rn.AZ, "providerID", rn.ProviderID)
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

	if config.Spec.AWS != nil {
		log.Info("cleaning up AWS resources")
		buildPlatform := r.PlatformBuilder
		if buildPlatform == nil {
			buildPlatform = defaultPlatformBuilder
		}
		p, err := buildPlatform(ctx, r.Client, config)
		if err != nil {
			log.Error(err, "unable to build AWS platform for cleanup, skipping cloud resource cleanup")
		} else if _, err := p.DiscoverEndpoints(ctx); err != nil {
			log.Error(err, "unable to discover endpoints for cleanup, skipping cloud resource cleanup")
		} else if err := p.Cleanup(ctx); err != nil {
			return ctrl.Result{}, fmt.Errorf("cleaning up AWS resources: %w", err)
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
	logf.FromContext(ctx).Error(fmt.Errorf("%s: %s", reason, message), "setting degraded status")

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
		Named("cudnbgpconfig").
		Complete(r)
}
