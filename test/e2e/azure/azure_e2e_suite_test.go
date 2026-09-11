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

package azure_e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	ginkgotypes "github.com/onsi/ginkgo/v2/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
	azureplatform "github.com/openshift/bgp-cloud-connector/internal/platform/azure"
)

var (
	k8sClient client.Client
	// A typed clientset as well, because pod logs are not something a
	// controller-runtime client can fetch, and the operator's log is
	// the first thing worth having when a spec fails.
	clientset *kubernetes.Clientset

	// The Azure clients are the SDK's own rather than the operator's
	// backend, for the reason the AWS suite talks to EC2 directly: a
	// suite that observes through the code under test cannot see a
	// fault in that code.
	peeringClient *armnetwork.VirtualHubBgpConnectionsClient
	// The singular client is the mutating one: the plural lists.
	peeringMutator *armnetwork.VirtualHubBgpConnectionClient
	nicClient      *armnetwork.InterfacesClient

	bgpConfig  *networkingapi.BGPCloudConfiguration
	bgpRouting *networkingapi.BGPRouting

	clusterID string
	// Every peering the operator owns starts with this. Azure has no
	// tags on a BGP connection, so unlike AWS the name is the only
	// mark of ownership.
	peeringPrefix string
)

func TestAzureE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Azure E2E Suite")
}

var _ = BeforeSuite(func() {
	// See the note in the AWS suite: a profile generated per run cannot
	// live under test/e2e/manifests, so an explicit directory wins.
	manifestDir := os.Getenv("E2E_MANIFEST_DIR")
	profile := os.Getenv("E2E_PROFILE")
	if manifestDir == "" {
		Expect(profile).NotTo(BeEmpty(),
			"set E2E_PROFILE (e.g. make test-e2e-azure my-cluster) or E2E_MANIFEST_DIR")
		manifestDir = filepath.Join("..", "..", "..", "test", "e2e", "manifests", profile)
	}

	GinkgoWriter.Printf("e2e profile directory: %s\n", manifestDir)
	By("loading BGPCloudConfiguration manifest from " + manifestDir)
	bgpConfig = &networkingapi.BGPCloudConfiguration{}
	loadManifest(filepath.Join(manifestDir, "bgpcloudconfiguration.yaml"), bgpConfig)
	Expect(bgpConfig.Spec.Azure).NotTo(BeNil(), "profile BGPCloudConfiguration must have spec.azure")
	Expect(bgpConfig.Spec.Azure.SubscriptionID).NotTo(BeEmpty())
	Expect(bgpConfig.Spec.Azure.ResourceGroup).NotTo(BeEmpty())
	Expect(bgpConfig.Spec.Azure.RouteServerName).NotTo(BeEmpty())

	// The CRD forbids spec.bgp.peerGroups on a cloud platform and
	// requires them under Manual, so a cloud suite asserts against
	// status.peerGroups and nothing else.
	Expect(bgpConfig.Spec.BGP.PeerGroups).To(BeEmpty(),
		"an Azure profile must not declare peer groups: the operator discovers them")

	By("loading BGPRouting manifest from " + manifestDir)
	bgpRouting = &networkingapi.BGPRouting{}
	loadManifest(filepath.Join(manifestDir, "bgprouting.yaml"), bgpRouting)

	By("building kubernetes client")
	scheme := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(networkingapi.AddToScheme(scheme)).To(Succeed())
	addUnstructuredTypes(scheme)

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	Expect(err).NotTo(HaveOccurred())
	k8sClient, err = client.New(restCfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())
	clientset, err = kubernetes.NewForConfig(restCfg)
	Expect(err).NotTo(HaveOccurred())

	By("building Azure clients using the default credential chain")
	// DefaultAzureCredential covers both callers: in prow hack/azure/ci.sh
	// has logged in as a service principal and pointed AZURE_CONFIG_DIR at
	// the run's scratch directory, which the CLI credential honours; at a
	// desk it is an ordinary az login.
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	Expect(err).NotTo(HaveOccurred())
	factory, err := armnetwork.NewClientFactory(bgpConfig.Spec.Azure.SubscriptionID, cred, nil)
	Expect(err).NotTo(HaveOccurred())
	peeringClient = factory.NewVirtualHubBgpConnectionsClient()
	peeringMutator = factory.NewVirtualHubBgpConnectionClient()
	nicClient = factory.NewInterfacesClient()

	By("reading cluster infrastructure name")
	infra := &unstructured.Unstructured{}
	infra.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "config.openshift.io", Version: "v1", Kind: "Infrastructure",
	})
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: "cluster"}, infra)).To(Succeed())
	name, found, err := unstructured.NestedString(infra.Object, "status", "infrastructureName")
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue())
	clusterID = name
	peeringPrefix = clusterID + "-bgp-"

	// Start from a known state, and only here: there is deliberately no
	// AfterSuite.
	//
	// Cleaning up on the way out would destroy the one thing worth
	// having after a failure. Ginkgo skips the remaining specs in an
	// Ordered container once one fails, so the deletion spec does not
	// run, and whatever broke is left standing for you to look at. An
	// AfterSuite would bulldoze exactly that.
	//
	// Cleaning up here instead still makes the suite re-runnable, and
	// does so in the case an AfterSuite cannot reach anyway: a run
	// killed by Ctrl-C, by go test's timeout, or by a panic never gets
	// to one.
	//
	// So a passing run leaves the cluster tidy because the deletion
	// spec removed everything, which is the behaviour it asserts; a
	// failing run leaves it dirty on purpose; and the next run starts
	// clean either way.
	By("removing anything a previous run left behind")
	cleanupE2EObjects(context.Background())
})

// cleanupE2EObjects removes everything the suite creates and treats
// "not there" as success, so it is safe to call on a clean cluster and
// safe to call twice.
//
// The order is the one the finalizers require: the configuration
// refuses to go while a routing CR exists, so routing goes first. The
// operator has to be running for either finalizer to clear, which is
// why this never force-removes one -- a cleanup that strips finalizers
// would leave the Azure peerings behind and hide the fact that the
// operator's own cleanup is broken.
func cleanupE2EObjects(ctx context.Context) {
	routing := &networkingapi.BGPRouting{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: bgpRouting.Name}, routing); err == nil {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, routing))).To(Succeed())
		waitGone(ctx, routing.Name, func() client.Object { return &networkingapi.BGPRouting{} })
	}

	config := &networkingapi.BGPCloudConfiguration{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, config); err == nil {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, config))).To(Succeed())
		// Generous: the operator removes the Azure peerings before it
		// clears this finalizer, one write at a time, and each one is
		// minutes.
		waitGone(ctx, config.Name, func() client.Object { return &networkingapi.BGPCloudConfiguration{} })
	}

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: bgpRouting.Spec.Network.Name}}
	Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, ns))).To(Succeed())
	Eventually(func(g Gomega) {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: ns.Name}, &corev1.Namespace{})
		g.Expect(err).To(HaveOccurred(), "namespace %s is still there", ns.Name)
		g.Expect(client.IgnoreNotFound(err)).To(Succeed())
	}).WithTimeout(cleanupTimeout).WithPolling(5 * time.Second).Should(Succeed())
}

func waitGone(ctx context.Context, name string, empty func() client.Object) {
	Eventually(func(g Gomega) {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, empty())
		g.Expect(err).To(HaveOccurred(), "%s is still there", name)
		g.Expect(client.IgnoreNotFound(err)).To(Succeed())
	}).WithTimeout(cleanupTimeout).WithPolling(5 * time.Second).Should(Succeed())
}

func loadManifest(path string, obj runtime.Object) {
	data, err := os.ReadFile(path)
	Expect(err).NotTo(HaveOccurred(), "reading manifest %s", path)
	Expect(yaml.NewYAMLOrJSONDecoder(
		bytes.NewReader(data), 4096,
	).Decode(obj)).To(Succeed(), "decoding manifest %s", path)
}

func addUnstructuredTypes(s *runtime.Scheme) {
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "frrk8s.metallb.io", Version: "v1beta1", Kind: "FRRConfiguration"},
		{Group: "k8s.ovn.org", Version: "v1", Kind: "ClusterUserDefinedNetwork"},
		{Group: "k8s.ovn.org", Version: "v1", Kind: "RouteAdvertisements"},
	} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
}

// observedPeering is what Azure reports for one BGP connection. The
// provisioning state is carried because a connection Azure failed to
// apply keeps its name, peer IP and ASN, so those three alone cannot
// tell a working peering from a dead one.
type observedPeering struct {
	Name              string
	PeerIP            string
	PeerASN           int64
	ProvisioningState string
}

// managedPeerings lists the peerings on the Route Server that belong to
// this cluster. Ownership is by name: Azure BGP connections carry no
// tags, so there is nothing else to key on.
func managedPeerings(ctx context.Context) ([]observedPeering, error) {
	var out []observedPeering
	pager := peeringClient.NewListPager(
		bgpConfig.Spec.Azure.ResourceGroup, bgpConfig.Spec.Azure.RouteServerName, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, c := range page.Value {
			if c == nil || c.Name == nil || !strings.HasPrefix(*c.Name, peeringPrefix) {
				continue
			}
			p := observedPeering{Name: *c.Name}
			if c.Properties != nil {
				if c.Properties.PeerIP != nil {
					p.PeerIP = *c.Properties.PeerIP
				}
				if c.Properties.PeerAsn != nil {
					p.PeerASN = *c.Properties.PeerAsn
				}
				if c.Properties.ProvisioningState != nil {
					p.ProvisioningState = string(*c.Properties.ProvisioningState)
				}
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// routerNodes are the nodes the operator peers, selected exactly as it
// selects them.
func routerNodes(ctx context.Context) ([]corev1.Node, error) {
	nodes := &corev1.NodeList{}
	if err := k8sClient.List(ctx, nodes, client.MatchingLabels(bgpConfig.Spec.RouterNodeSelector)); err != nil {
		return nil, err
	}
	return nodes.Items, nil
}

func nodeInternalIP(node *corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}

// nicForNode is the network interface attached to the node's VM, found
// the way the operator finds it: by the VM's ARM resource id recorded
// on the interface, so no naming convention has to hold.
func nicForNode(ctx context.Context, node *corev1.Node) (*armnetwork.Interface, error) {
	vm, err := azureplatform.ParseProviderID(node.Spec.ProviderID)
	if err != nil {
		return nil, err
	}
	pager := nicClient.NewListPager(vm.ResourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, nic := range page.Value {
			if nic == nil || nic.Properties == nil || nic.Properties.VirtualMachine == nil {
				continue
			}
			if nic.Properties.VirtualMachine.ID == nil {
				continue
			}
			if strings.EqualFold(*nic.Properties.VirtualMachine.ID, vm.ID) {
				return nic, nil
			}
		}
	}
	return nil, nil
}

// deletePeering removes one BGP connection behind the operator's back so
// a test can watch it be rebuilt. It blocks until Azure has finished,
// because Azure applies one write to a Route Server at a time and a
// second call arriving while the first is in flight is refused with
// ConflictError rather than queued.
func deletePeering(ctx context.Context, name string) error {
	poller, err := peeringMutator.BeginDelete(ctx,
		bgpConfig.Spec.Azure.ResourceGroup, bgpConfig.Spec.Azure.RouteServerName, name, nil)
	if err != nil {
		return err
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}

// setForwarding rewrites enableIPForwarding on a node's interface, so a
// test can turn it off and watch the operator put it back.
func setForwarding(ctx context.Context, nic *armnetwork.Interface, enabled bool) error {
	nic.Properties.EnableIPForwarding = to.Ptr(enabled)
	poller, err := nicClient.BeginCreateOrUpdate(ctx,
		resourceGroupOf(*nic.ID), *nic.Name, *nic, nil)
	if err != nil {
		return err
	}
	_, err = poller.PollUntilDone(ctx, nil)
	return err
}

// resourceGroupOf pulls the group out of an ARM resource id, which is
// the only place an interface records the group it lives in.
//
//	/subscriptions/<sub>/resourceGroups/<rg>/providers/...
func resourceGroupOf(id string) string {
	parts := strings.Split(id, "/")
	for i := 0; i+1 < len(parts); i++ {
		if strings.EqualFold(parts[i], "resourceGroups") {
			return parts[i+1]
		}
	}
	return ""
}

// ReportAfterEach prints everything worth having when a spec fails.
//
// In CI this is the only record that survives. hack/ci-e2e-azure.sh tears
// the estate down whatever the result, which deletes the configuration and
// scales the operator to zero, so by the time prow gathers artefacts the
// conditions, the peerings and the pod log have all gone. Leaving objects
// behind for someone to inspect works at a desk and not here.
//
// It is deliberately everything at once rather than the one thing that
// looked relevant. A run costs about two hours end to end, so a diagnostic
// that sends you round again to ask the next question is worth very little.
//
// Nothing here asserts. A failure inside a reporting node would replace the
// failure you are trying to read.
var _ = ReportAfterEach(func(report SpecReport) {
	if !report.Failed() {
		return
	}
	dumpEverything(report.LeafNodeText)
})

// ReportAfterSuite covers the case ReportAfterEach cannot: a failure in
// BeforeSuite, where no spec ever runs and nothing above fires. The
// start-of-run cleanup waiting out a finalizer is the likely one, and a
// silent failure there costs a whole run to diagnose.
var _ = ReportAfterSuite("diagnostics", func(report Report) {
	if report.SuiteSucceeded {
		return
	}
	for _, spec := range report.SpecReports {
		if spec.State.Is(ginkgotypes.SpecStatePassed | ginkgotypes.SpecStateFailed) {
			return // a spec ran, so ReportAfterEach has already spoken
		}
	}
	dumpEverything("suite setup")
})

// dumpEverything is deliberately everything at once rather than the one
// thing that looked relevant. A run costs about two hours end to end, so a
// diagnostic that sends you round again to ask the next question is worth
// very little.
//
// Nothing here asserts, and every step tolerates the clients being nil: a
// failure inside a reporting node would replace the failure you are trying
// to read.
func dumpEverything(what string) {
	if k8sClient == nil || clientset == nil {
		GinkgoWriter.Printf("diagnostics unavailable: the clients were never built\n")
		return
	}
	ctx := context.Background()
	say("================ diagnostics for %s ================", what)
	dumpOperator(ctx)
	dumpConfiguration(ctx)
	dumpRouting(ctx)
	dumpCredentials(ctx)
	dumpNodes(ctx)
	dumpFRR(ctx)
	dumpAzure(ctx)
	dumpWarnings(ctx)
	dumpOperatorLog(ctx)
	say("================ end diagnostics ================")
}

func say(format string, args ...any) {
	GinkgoWriter.Printf(format+"\n", args...)
}

// dumpOperator names the build under test. A surprising result is often a
// stale image rather than a bug.
func dumpOperator(ctx context.Context) {
	say("--- operator ---")
	dep, err := clientset.AppsV1().Deployments(operatorNamespace).
		Get(ctx, "openshift-bgp-cloud-connector-controller-manager", metav1.GetOptions{})
	if err != nil {
		say("  cannot read the deployment: %v", err)
		return
	}
	say("  replicas: %d ready, %d desired", dep.Status.ReadyReplicas, *dep.Spec.Replicas)
	for _, c := range dep.Spec.Template.Spec.Containers {
		say("  container %s: %s", c.Name, c.Image)
	}
}

func dumpConfiguration(ctx context.Context) {
	say("--- BGPCloudConfiguration ---")
	cfg := &networkingapi.BGPCloudConfiguration{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, cfg); err != nil {
		say("  not there: %v", err)
		return
	}
	// Status only. The spec carries the subscription id and prow logs
	// for openshift repositories are public.
	say("  generation %d, phase %s", cfg.Generation, cfg.Status.Phase)
	for _, c := range cfg.Status.Conditions {
		say("  %-30s %-7s obsGen=%d %s: %s", c.Type, c.Status, c.ObservedGeneration, c.Reason, c.Message)
	}
	if len(cfg.Status.PeerGroups) == 0 {
		say("  status.peerGroups: empty")
	}
	for _, g := range cfg.Status.PeerGroups {
		for _, n := range g.Neighbors {
			say("  peer group %s: %s asn=%d multihop=%v", g.Key, n.Address, n.RemoteASN, n.EBGPMultiHop)
		}
	}
}

func dumpRouting(ctx context.Context) {
	say("--- BGPRouting ---")
	routing := &networkingapi.BGPRouting{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: bgpRouting.Name}, routing); err != nil {
		say("  not there: %v", err)
		return
	}
	say("  phase %s", routing.Status.Phase)
	for _, c := range routing.Status.Conditions {
		say("  %-30s %-7s %s: %s", c.Type, c.Status, c.Reason, c.Message)
	}
	for _, kind := range []string{"ClusterUserDefinedNetwork", "RouteAdvertisements"} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(schema.GroupVersionKind{Group: "k8s.ovn.org", Version: "v1", Kind: kind})
		name := routeAdvertisementName
		if kind == "ClusterUserDefinedNetwork" {
			name = "cluster-udn-" + bgpRouting.Spec.Network.Name
		}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, u); err != nil {
			say("  %s/%s: %v", kind, name, err)
			continue
		}
		conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		say("  %s/%s: %d conditions", kind, name, len(conds))
		for _, c := range conds {
			if m, ok := c.(map[string]any); ok {
				say("    %v %v %v", m["type"], m["status"], m["reason"])
			}
		}
	}
}

// dumpCredentials is the first fork in the road: no request at all means
// the operator failed before it resolved a credential, which is a very
// different problem from one it resolved and could not use.
func dumpCredentials(ctx context.Context) {
	say("--- credentials ---")
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cloudcredential.openshift.io", Version: "v1", Kind: "CredentialsRequest",
	})
	err := k8sClient.Get(ctx, types.NamespacedName{
		Name: "bgp-cloud-connector-azure", Namespace: "openshift-cloud-credential-operator",
	}, cr)
	if err != nil {
		say("  CredentialsRequest: %v", err)
	} else {
		provisioned, _, _ := unstructured.NestedBool(cr.Object, "status", "provisioned")
		say("  CredentialsRequest: provisioned=%v", provisioned)
	}
	// Keys only, never values.
	secret, err := clientset.CoreV1().Secrets(operatorNamespace).
		Get(ctx, "bgp-cloud-connector-azure-credentials", metav1.GetOptions{})
	if err != nil {
		say("  secret: %v", err)
		return
	}
	keys := make([]string, 0, len(secret.Data))
	for k := range secret.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	say("  secret keys: %s", strings.Join(keys, " "))
}

func dumpNodes(ctx context.Context) {
	say("--- router nodes (selector %v) ---", bgpConfig.Spec.RouterNodeSelector)
	nodes, err := routerNodes(ctx)
	if err != nil {
		say("  cannot list: %v", err)
		return
	}
	if len(nodes) == 0 {
		say("  none matched, which is why nothing was peered")
	}
	for i := range nodes {
		say("  %s ip=%s provider=%s", nodes[i].Name, nodeInternalIP(&nodes[i]), nodes[i].Spec.ProviderID)
	}
}

func dumpFRR(ctx context.Context) {
	say("--- FRR ---")
	cfgs := &unstructured.UnstructuredList{}
	cfgs.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "frrk8s.metallb.io", Version: "v1beta1", Kind: "FRRConfigurationList",
	})
	if err := k8sClient.List(ctx, cfgs, client.InNamespace(frrNamespace)); err != nil {
		say("  cannot list FRRConfigurations: %v", err)
	} else {
		say("  %d FRRConfiguration(s)", len(cfgs.Items))
		for _, c := range cfgs.Items {
			say("    %s", c.GetName())
		}
	}
	sessions := &unstructured.UnstructuredList{}
	sessions.SetGroupVersionKind(bgpSessionStateGVK)
	if err := k8sClient.List(ctx, sessions, client.InNamespace(frrNamespace)); err != nil {
		say("  cannot list BGPSessionStates: %v", err)
		return
	}
	say("  %d BGP session(s)", len(sessions.Items))
	for _, s := range sessions.Items {
		status, _, _ := unstructured.NestedString(s.Object, "status", "bgpStatus")
		peer, _, _ := unstructured.NestedString(s.Object, "status", "peer")
		node, _, _ := unstructured.NestedString(s.Object, "status", "node")
		say("    %s -> %s %s", node, peer, status)
	}
}

// dumpAzure is what the cloud says, as opposed to what the cluster believes.
// The two disagreeing is the whole point of an e2e.
func dumpAzure(ctx context.Context) {
	say("--- Azure ---")
	peerings, err := managedPeerings(ctx)
	if err != nil {
		say("  cannot list peerings: %v", err)
	} else {
		say("  %d peering(s) named %s*", len(peerings), peeringPrefix)
		for _, p := range peerings {
			say("    %s ip=%s asn=%d %s", p.Name, p.PeerIP, p.PeerASN, p.ProvisioningState)
		}
	}
	nodes, nodeErr := routerNodes(ctx)
	if nodeErr != nil {
		return
	}
	for i := range nodes {
		nic, nicErr := nicForNode(ctx, &nodes[i])
		switch {
		case nicErr != nil:
			say("    %s: cannot read interface: %v", nodes[i].Name, nicErr)
		case nic == nil:
			say("    %s: no interface found", nodes[i].Name)
		case nic.Properties == nil || nic.Properties.EnableIPForwarding == nil:
			say("    %s: %s forwarding unset", nodes[i].Name, *nic.Name)
		default:
			say("    %s: %s forwarding=%v", nodes[i].Name, *nic.Name, *nic.Properties.EnableIPForwarding)
		}
	}
}

func dumpWarnings(ctx context.Context) {
	say("--- warning events in %s ---", operatorNamespace)
	events, err := clientset.CoreV1().Events(operatorNamespace).List(ctx, metav1.ListOptions{
		FieldSelector: "type=Warning",
	})
	if err != nil {
		say("  cannot list: %v", err)
		return
	}
	if len(events.Items) == 0 {
		say("  none")
	}
	for _, e := range events.Items {
		say("  %s %s/%s: %s", e.Reason, e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Message)
	}
}

// dumpOperatorLog prints the tail of whichever manager pod is running, and
// the previous container's tail too when it has restarted, because the
// interesting failure is often the one that caused the restart.
func dumpOperatorLog(ctx context.Context) {
	say("--- operator log ---")
	pods, err := clientset.CoreV1().Pods(operatorNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		say("  cannot list pods: %v", err)
		return
	}
	tail := int64(300)
	for _, pod := range pods.Items {
		if !strings.Contains(pod.Name, "controller-manager") {
			continue
		}
		restarted := false
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == "manager" && cs.RestartCount > 0 {
				restarted = true
			}
		}
		for _, previous := range []bool{false, true} {
			if previous && !restarted {
				continue
			}
			raw, logErr := clientset.CoreV1().Pods(operatorNamespace).GetLogs(pod.Name, &corev1.PodLogOptions{
				Container: "manager", TailLines: &tail, Previous: previous,
			}).DoRaw(ctx)
			if logErr != nil {
				say("  cannot read log for %s (previous=%v): %v", pod.Name, previous, logErr)
				continue
			}
			say("  last %d lines of %s (previous=%v):\n%s", tail, pod.Name, previous, raw)
		}
	}
}
