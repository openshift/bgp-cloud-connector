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
	"os"
	"strings"
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

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
	"github.com/openshift/bgp-cloud-connector/internal/platform"
	awsplatform "github.com/openshift/bgp-cloud-connector/internal/platform/aws"
	azureplatform "github.com/openshift/bgp-cloud-connector/internal/platform/azure"
	gcpplatform "github.com/openshift/bgp-cloud-connector/internal/platform/gcp"
)

// +kubebuilder:rbac:groups=networking.openshift.io,resources=bgpcloudconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.openshift.io,resources=bgpcloudconfigurations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.openshift.io,resources=bgpcloudconfigurations/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.openshift.io,resources=bgproutings,verbs=get;list;watch
// +kubebuilder:rbac:groups=operator.openshift.io,resources=networks,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=frrk8s.metallb.io,resources=frrconfigurations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=config.openshift.io,resources=infrastructures,verbs=get
// +kubebuilder:rbac:groups=cloudcredential.openshift.io,resources=credentialsrequests,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=secrets,resourceNames=bgp-cloud-connector-aws-credentials,verbs=get,namespace=openshift-bgp-cloud-connector
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=create;delete;update;patch

type PlatformBuilderFunc func(ctx context.Context, c client.Client, config *networkingapi.BGPCloudConfiguration) (platform.CloudPlatform, error)

type BGPCloudConfigurationReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	PlatformBuilder PlatformBuilderFunc
}

func (r *BGPCloudConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	config := &networkingapi.BGPCloudConfiguration{}
	if err := r.Get(ctx, req.NamespacedName, config); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	baselineStatus := config.Status.DeepCopy()

	if config.Name != SingletonName {
		return r.setDegraded(ctx, config, *baselineStatus, networkingapi.ConditionNetworkOperatorPatched,
			"InvalidName", fmt.Sprintf("BGPCloudConfiguration must be named %q, got %q", SingletonName, config.Name))
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

	config.Status.Phase = networkingapi.PhaseConfiguring
	config.Status.ObservedGeneration = config.Generation

	// Phase 1: Patch Network Operator
	log.Info("Phase 1: patching Network operator")
	if err := PatchNetworkOperator(ctx, r.Client); err != nil {
		return r.setDegraded(ctx, config, *baselineStatus, networkingapi.ConditionNetworkOperatorPatched,
			"PatchFailed", fmt.Sprintf("failed to patch Network operator: %v", err))
	}
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:               networkingapi.ConditionNetworkOperatorPatched,
		Status:             metav1.ConditionTrue,
		Reason:             "Patched",
		Message:            "Network operator patched with FRR and routeAdvertisements",
		ObservedGeneration: config.Generation,
	})

	// Phase 2: Wait for FRR
	log.Info("Phase 2: checking FRR readiness")
	ready, err := IsFRRReady(ctx, r.Client)
	if err != nil {
		return r.setDegraded(ctx, config, *baselineStatus, networkingapi.ConditionFRRNamespaceReady,
			"CheckFailed", fmt.Sprintf("failed to check FRR readiness: %v", err))
	}
	if !ready {
		if err := r.patchConfigStatus(ctx, config, *baselineStatus, func(c *networkingapi.BGPCloudConfiguration) {
			c.Status.Phase = networkingapi.PhaseConfiguring
			c.Status.ObservedGeneration = c.Generation
			meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
				Type:               networkingapi.ConditionFRRNamespaceReady,
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
		Type:               networkingapi.ConditionFRRNamespaceReady,
		Status:             metav1.ConditionTrue,
		Reason:             "Ready",
		Message:            "FRR namespace and pods are running",
		ObservedGeneration: config.Generation,
	})

	config.Status.PeerGroups = nil
	if config.Spec.Platform == networkingapi.PlatformManual {
		meta.RemoveStatusCondition(&config.Status.Conditions, networkingapi.ConditionCloudEndpointsDiscovered)
		meta.RemoveStatusCondition(&config.Status.Conditions, networkingapi.ConditionCloudResourcesReconciled)
		meta.RemoveStatusCondition(&config.Status.Conditions, networkingapi.ConditionCompleteNodeInventory)
	}

	// Build the cloud platform once if configured (used in Phases 3 and 5)
	var cloudPlatform platform.CloudPlatform
	var discoveryResult *platform.DiscoveryResult
	if config.Spec.Platform != networkingapi.PlatformManual {
		buildPlatform := r.PlatformBuilder
		if buildPlatform == nil {
			buildPlatform = defaultPlatformBuilder
		}
		p, err := buildPlatform(ctx, r.Client, config)
		if err != nil {
			// We will not reach Phase 5, so drop stale cloud-resource conditions from
			// a previous reconcile; leaving True here would contradict the degraded
			// cloud condition. This path returns before completeReconcile, so the
			// NodesIncomplete timer is unaffected.
			meta.RemoveStatusCondition(&config.Status.Conditions, networkingapi.ConditionCompleteNodeInventory)
			meta.RemoveStatusCondition(&config.Status.Conditions, networkingapi.ConditionCloudResourcesReconciled)
			// Asking the cluster to mint credentials and waiting for it
			// is not a fault, and is reported like Phase 2's wait rather
			// than through setDegraded.
			if errors.Is(err, platform.ErrCredentialsPending) {
				if err := r.patchConfigStatus(ctx, config, *baselineStatus, func(c *networkingapi.BGPCloudConfiguration) {
					c.Status.Phase = networkingapi.PhaseConfiguring
					c.Status.ObservedGeneration = c.Generation
					meta.SetStatusCondition(&c.Status.Conditions, metav1.Condition{
						Type:   networkingapi.ConditionCloudEndpointsDiscovered,
						Status: metav1.ConditionFalse,
						Reason: "WaitingForCloudCredentials",
						// What the resolver said, not a fixed sentence:
						// on a cluster that federates this wait does not
						// end by itself, and this is the only place that
						// is reported.
						Message:            capitalise(err.Error()),
						ObservedGeneration: c.Generation,
					})
				}); err != nil {
					return ctrl.Result{}, err
				}
				log.Info("cloud credentials not available yet, requeueing")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			var credErr *platform.CredentialError
			if errors.As(err, &credErr) {
				return r.setDegraded(ctx, config, *baselineStatus, networkingapi.ConditionCloudEndpointsDiscovered,
					ReasonCloudCredentialsInvalid, credErr.Error())
			}
			return r.setDegraded(ctx, config, *baselineStatus, networkingapi.ConditionCloudEndpointsDiscovered,
				ReasonCloudDiscoveryFailed, fmt.Sprintf("failed to build the cloud platform: %v", err))
		}
		cloudPlatform = p

		// Phase 3: Discover the cloud's BGP endpoints
		log.Info("Phase 3: discovering cloud BGP endpoints")
		discoveryResult, err = cloudPlatform.DiscoverEndpoints(ctx)
		if err != nil {
			meta.RemoveStatusCondition(&config.Status.Conditions, networkingapi.ConditionCompleteNodeInventory)
			meta.RemoveStatusCondition(&config.Status.Conditions, networkingapi.ConditionCloudResourcesReconciled)
			var notFoundErr *awsplatform.RouteServerNotFoundError
			if errors.As(err, &notFoundErr) {
				return r.setDegraded(ctx, config, *baselineStatus, networkingapi.ConditionCloudEndpointsDiscovered,
					ReasonRouteServerNotFound, notFoundErr.Error())
			}
			return r.setDegraded(ctx, config, *baselineStatus, networkingapi.ConditionCloudEndpointsDiscovered,
				ReasonCloudDiscoveryFailed, fmt.Sprintf("failed to discover cloud BGP endpoints: %v", err))
		}
		config.Status.PeerGroups = peerGroupsToStatus(discoveryResult.PeerGroups)
		meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
			Type:               networkingapi.ConditionCloudEndpointsDiscovered,
			Status:             metav1.ConditionTrue,
			Reason:             "Discovered",
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
		return r.setDegraded(ctx, config, *baselineStatus, networkingapi.ConditionFRRConfigurationApplied,
			"ApplyFailed", fmt.Sprintf("failed to apply FRR configurations: %v", err))
	}
	meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
		Type:               networkingapi.ConditionFRRConfigurationApplied,
		Status:             metav1.ConditionTrue,
		Reason:             "Applied",
		Message:            fmt.Sprintf("Applied %d FRRConfiguration(s)", frrCount),
		ObservedGeneration: config.Generation,
	})

	// Phase 5: Reconcile cloud resources (if configured)
	readyPhase := true
	if cloudPlatform != nil {
		log.Info("Phase 5: reconciling cloud resources")
		nodes, incomplete, err := r.listRouterNodes(ctx, config)
		if err != nil {
			return r.setDegraded(ctx, config, *baselineStatus, networkingapi.ConditionCloudResourcesReconciled,
				"CloudReconcileFailed", fmt.Sprintf("failed to list router nodes: %v", err))
		}
		readyPhase = len(incomplete) == 0
		if readyPhase {
			meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
				Type:               networkingapi.ConditionCompleteNodeInventory,
				Status:             metav1.ConditionTrue,
				Reason:             "Complete",
				Message:            "all selected router nodes have IP, zone, and providerID",
				ObservedGeneration: config.Generation,
			})
		} else {
			// TODO: emit Warning Event on transition to incomplete (once EventRecorder exists):
			// r.Recorder.Event(config, corev1.EventTypeWarning, "NodesIncomplete", formatIncompleteNodesMessage(incomplete))
			meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
				Type:               networkingapi.ConditionCompleteNodeInventory,
				Status:             metav1.ConditionFalse,
				Reason:             "NodesIncomplete",
				Message:            formatIncompleteNodesMessage(incomplete),
				ObservedGeneration: config.Generation,
			})
		}
		if err := cloudPlatform.ReconcileNodes(ctx, nodes); err != nil {
			return r.setDegraded(ctx, config, *baselineStatus, networkingapi.ConditionCloudResourcesReconciled,
				"CloudReconcileFailed", fmt.Sprintf("failed to reconcile cloud resources: %v", err))
		}
		if readyPhase {
			meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
				Type:               networkingapi.ConditionCloudResourcesReconciled,
				Status:             metav1.ConditionTrue,
				Reason:             "Reconciled",
				Message:            "Cloud BGP peerings and router node settings reconciled",
				ObservedGeneration: config.Generation,
			})
		} else {
			meta.SetStatusCondition(&config.Status.Conditions, metav1.Condition{
				Type:               networkingapi.ConditionCloudResourcesReconciled,
				Status:             metav1.ConditionFalse,
				Reason:             "NodesIncomplete",
				Message:            "Cloud resources reconciled only for complete router nodes",
				ObservedGeneration: config.Generation,
			})
		}
	}

	return r.completeReconcile(ctx, config, *baselineStatus, readyPhase)
}

func (r *BGPCloudConfigurationReconciler) completeReconcile(
	ctx context.Context,
	config *networkingapi.BGPCloudConfiguration,
	baselineStatus networkingapi.BGPCloudConfigurationStatus,
	readyPhase bool,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !readyPhase {
		cond := meta.FindStatusCondition(config.Status.Conditions, networkingapi.ConditionCompleteNodeInventory)
		if cond != nil && cond.Status == metav1.ConditionFalse &&
			!cond.LastTransitionTime.IsZero() && time.Since(cond.LastTransitionTime.Time) >= 5*time.Minute {
			return r.setDegraded(ctx, config, baselineStatus, networkingapi.ConditionCompleteNodeInventory,
				"NodesIncomplete", cond.Message)
		}
	}

	if err := r.patchConfigStatus(ctx, config, baselineStatus, func(c *networkingapi.BGPCloudConfiguration) {
		if readyPhase {
			c.Status.Phase = networkingapi.PhaseReady
		} else {
			c.Status.Phase = networkingapi.PhaseConfiguring
		}
	}); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("reconciliation complete", "phase", config.Status.Phase)
	if !readyPhase {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// peerGroupsToStatus reports the peering plan, which is what FRR was told to
// peer with and therefore the thing worth reading.
func peerGroupsToStatus(groups []platform.PeerGroup) []networkingapi.PeerGroupStatus {
	if len(groups) == 0 {
		return nil
	}
	out := make([]networkingapi.PeerGroupStatus, 0, len(groups))
	for _, g := range groups {
		pg := networkingapi.PeerGroupStatus{
			Key:          g.Key,
			NodeSelector: g.NodeSelector,
		}
		for _, n := range g.Neighbors {
			pg.Neighbors = append(pg.Neighbors, networkingapi.BGPNeighbor{
				Address:      n.Address,
				RemoteASN:    n.ASN,
				EBGPMultiHop: n.EBGPMultiHop,
			})
		}
		out = append(out, pg)
	}
	return out
}

// defaultPlatformBuilder constructs the cloud platform named by spec.platform.
// PlatformManual never reaches here: the controller skips platform
// construction entirely for it.
func defaultPlatformBuilder(ctx context.Context, c client.Client, config *networkingapi.BGPCloudConfiguration) (platform.CloudPlatform, error) {
	switch config.Spec.Platform {
	case networkingapi.PlatformAWS:
		return buildAWSPlatform(ctx, c, config)
	case networkingapi.PlatformAzure:
		return buildAzurePlatform(ctx, c, config)
	case networkingapi.PlatformGCP:
		return buildGCPPlatform(ctx, c, config)
	default:
		return nil, fmt.Errorf("no platform implementation for %q", config.Spec.Platform)
	}
}

func buildAWSPlatform(ctx context.Context, c client.Client, config *networkingapi.BGPCloudConfiguration) (platform.CloudPlatform, error) {
	awsSpec := config.Spec.AWS

	clusterID, err := getInfrastructureName(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("reading cluster infrastructure name: %w", err)
	}

	// May report platform.ErrCredentialsPending, which Reconcile waits
	// out rather than treating as a fault.
	creds, err := awsplatform.ResolveCredentials(ctx, c, OperatorNamespace(), awsSpec.Region)
	if err != nil {
		return nil, err
	}

	cfg := awsplatform.Config{
		Region:            awsSpec.Region,
		RouteServerIDs:    awsSpec.RouteServerIDs,
		LocalASN:          config.Spec.BGP.LocalASN,
		LivenessDetection: string(config.Spec.BGP.LivenessDetection),
		ClusterID:         clusterID,
		Credentials:       creds,
	}

	return awsplatform.New(ctx, cfg)
}

func buildAzurePlatform(ctx context.Context, c client.Client, config *networkingapi.BGPCloudConfiguration) (platform.CloudPlatform, error) {
	azureSpec := config.Spec.Azure

	clusterID, err := getInfrastructureName(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("reading cluster infrastructure name: %w", err)
	}

	return azureplatform.New(azureplatform.Config{
		SubscriptionID:  azureSpec.SubscriptionID,
		ResourceGroup:   azureSpec.ResourceGroup,
		RouteServerName: azureSpec.RouteServerName,
		NICClientID:     azureSpec.NetworkInterfaceClientID,
		LocalASN:        config.Spec.BGP.LocalASN,
		ClusterID:       clusterID,
	})
}

func buildGCPPlatform(ctx context.Context, c client.Client, config *networkingapi.BGPCloudConfiguration) (platform.CloudPlatform, error) {
	gcpSpec := config.Spec.GCP

	clusterID, err := getInfrastructureName(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("reading cluster infrastructure name: %w", err)
	}

	cfg := gcpplatform.Config{
		Project:         gcpSpec.Project,
		Region:          gcpSpec.Region,
		CloudRouterName: gcpSpec.CloudRouterName,
		NCCHubName:      gcpSpec.NCC.HubName,
		NCCSpokePrefix:  gcpSpec.NCC.SpokePrefix,
		SiteToSite:      gcpSpec.NCC.SiteToSiteDataTransfer,
		NestedVirt:      gcpSpec.EnableNestedVirtualization == nil || *gcpSpec.EnableNestedVirtualization,
		LocalASN:        config.Spec.BGP.LocalASN,
		ClusterID:       clusterID,
	}

	return gcpplatform.New(ctx, cfg)
}

// capitalise upper-cases the first letter, because Go error strings
// start lower case and condition messages read as sentences.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// OperatorNamespace is where the operator is running, which is where the
// cloud credential operator will put the secret it writes. OLM can
// install into a namespace of the administrator's choosing, so the
// Deployment passes it down; the constant covers running the manager
// from a desk.
func OperatorNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	return DefaultOperatorNamespace
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

func (r *BGPCloudConfigurationReconciler) listRouterNodes(ctx context.Context, config *networkingapi.BGPCloudConfiguration) (complete []platform.RouterNode, incomplete []platform.RouterNode, err error) {
	nodeList := &corev1.NodeList{}
	sel := labels.SelectorFromSet(config.Spec.RouterNodeSelector)
	if err := r.List(ctx, nodeList, client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, nil, err
	}

	complete = make([]platform.RouterNode, 0, len(nodeList.Items))
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
			incomplete = append(incomplete, rn)
			continue
		}
		complete = append(complete, rn)
	}
	return complete, incomplete, nil
}

func formatIncompleteNodesMessage(incomplete []platform.RouterNode) string {
	parts := make([]string, 0, len(incomplete))
	for _, n := range incomplete {
		var missing []string
		if n.PrivateIP == "" {
			missing = append(missing, "IP")
		}
		if n.Zone == "" {
			missing = append(missing, "zone")
		}
		if n.ProviderID == "" {
			missing = append(missing, "providerID")
		}
		parts = append(parts, n.Name+" (missing "+strings.Join(missing, "/")+")")
	}
	msg := fmt.Sprintf("%d router node(s) incomplete: %s", len(incomplete), strings.Join(parts, "; "))
	if len(msg) > 32768 {
		return msg[:32768]
	}
	return msg
}

func (r *BGPCloudConfigurationReconciler) reconcileDelete(ctx context.Context, config *networkingapi.BGPCloudConfiguration) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	baselineStatus := config.Status.DeepCopy()

	routingList := &networkingapi.BGPRoutingList{}
	if err := r.List(ctx, routingList); err != nil {
		return ctrl.Result{}, err
	}
	if len(routingList.Items) > 0 {
		log.Info("deletion blocked: BGPRouting CRs still exist", "count", len(routingList.Items))
		if err := r.reportDeletionBlocked(ctx, config, *baselineStatus, routingList.Items); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if config.Spec.Platform != networkingapi.PlatformManual {
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

func (r *BGPCloudConfigurationReconciler) setDegraded(
	ctx context.Context,
	config *networkingapi.BGPCloudConfiguration,
	baselineStatus networkingapi.BGPCloudConfigurationStatus,
	condType, reason, message string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	terminal := IsTerminalDegradedReason(reason)
	if terminal {
		log.Info("terminal degraded condition, not requeueing", "reason", reason, "message", message)
	} else {
		log.Error(fmt.Errorf("%s: %s", reason, message), "setting degraded status")
	}

	if err := r.patchConfigStatus(ctx, config, baselineStatus, func(c *networkingapi.BGPCloudConfiguration) {
		c.Status.Phase = networkingapi.PhaseDegraded
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
	if terminal {
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

// SetupWithManager watches only APIs that exist on every cluster the
// operator can be installed on. FRRConfiguration is not one of them: CNO
// creates that CRD in response to the patch this controller applies in
// Phase 1, so demanding it here would mean the manager could only start
// on a cluster where its own work had already been done. Drift on the
// FRRConfigurations we write is picked up by ResyncInterval instead.
func (r *BGPCloudConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingapi.BGPCloudConfiguration{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: SingletonName}}}
			},
		), builder.WithPredicates(nodeRelevantChangePredicate())).
		Watches(&networkingapi.BGPRouting{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, _ client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: SingletonName}}}
			},
		), builder.WithPredicates(predicate.Funcs{
			CreateFunc:  func(event.CreateEvent) bool { return true },
			DeleteFunc:  func(event.DeleteEvent) bool { return true },
			UpdateFunc:  func(event.UpdateEvent) bool { return false },
			GenericFunc: func(event.GenericEvent) bool { return false },
		})).
		Named("bgpcloudconfiguration").
		Complete(r)
}
