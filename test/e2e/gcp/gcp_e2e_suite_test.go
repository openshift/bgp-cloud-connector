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

package gcp_e2e

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
	ginkgotypes "github.com/onsi/ginkgo/v2/types"
	. "github.com/onsi/gomega"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/networkconnectivity/v1"
	"google.golang.org/api/option"
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
	gcpplatform "github.com/openshift/bgp-cloud-connector/internal/platform/gcp"
)

var (
	k8sClient client.Client
	// A typed clientset as well, because pod logs are not something a
	// controller-runtime client can fetch, and the operator's log is
	// the first thing worth having when a spec fails.
	clientset *kubernetes.Clientset

	// The Google clients are the SDK's own rather than the operator's,
	// for the reason the AWS suite talks to EC2 directly: a suite that
	// observes through the code under test cannot see a fault in it.
	computeSvc *compute.Service
	nccSvc     *networkconnectivity.Service

	bgpConfig  *networkingapi.BGPCloudConfiguration
	bgpRouting *networkingapi.BGPRouting

	clusterID string
)

func TestGCPE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "GCP E2E Suite")
}

var _ = BeforeSuite(func() {
	// See the note in the AWS suite: a profile generated per run cannot
	// live under test/e2e/manifests, so an explicit directory wins.
	manifestDir := os.Getenv("E2E_MANIFEST_DIR")
	profile := os.Getenv("E2E_PROFILE")
	if manifestDir == "" {
		Expect(profile).NotTo(BeEmpty(),
			"set E2E_PROFILE (e.g. make test-e2e-gcp my-cluster) or E2E_MANIFEST_DIR")
		manifestDir = filepath.Join("..", "..", "..", "test", "e2e", "manifests", profile)
	}
	GinkgoWriter.Printf("e2e profile directory: %s\n", manifestDir)

	By("loading BGPCloudConfiguration manifest from " + manifestDir)
	bgpConfig = &networkingapi.BGPCloudConfiguration{}
	loadManifest(filepath.Join(manifestDir, "bgpcloudconfiguration.yaml"), bgpConfig)
	Expect(bgpConfig.Spec.GCP).NotTo(BeNil(), "profile BGPCloudConfiguration must have spec.gcp")
	Expect(bgpConfig.Spec.GCP.Project).NotTo(BeEmpty())
	Expect(bgpConfig.Spec.GCP.Region).NotTo(BeEmpty())
	Expect(bgpConfig.Spec.GCP.CloudRouterName).NotTo(BeEmpty())
	Expect(bgpConfig.Spec.GCP.NCC.HubName).NotTo(BeEmpty())

	// The CRD forbids spec.bgp.peerGroups on a cloud platform and
	// requires them under Manual, so a cloud suite asserts against
	// status.peerGroups and nothing else.
	Expect(bgpConfig.Spec.BGP.PeerGroups).To(BeEmpty(),
		"a GCP profile must not declare peer groups: the operator discovers them")

	By("loading BGPRouting manifest from " + manifestDir)
	bgpRouting = &networkingapi.BGPRouting{}
	loadManifest(filepath.Join(manifestDir, "bgprouting.yaml"), bgpRouting)

	By("building kubernetes clients")
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

	By("building Google clients from the ambient credential")
	// Application Default Credentials is right for the suite even
	// though it is wrong for the operator: this runs where somebody has
	// a gcloud login, or in prow where hack/gcp/ci.sh has exported
	// GOOGLE_APPLICATION_CREDENTIALS from the cluster profile. The
	// operator cannot use it because a pod cannot reach the metadata
	// server, which is what internal/platform/gcp/credentials.go is for.
	ctx := context.Background()
	computeSvc, err = compute.NewService(ctx, option.WithScopes(compute.CloudPlatformScope))
	Expect(err).NotTo(HaveOccurred())
	nccSvc, err = networkconnectivity.NewService(ctx, option.WithScopes(compute.CloudPlatformScope))
	Expect(err).NotTo(HaveOccurred())

	By("reading cluster infrastructure name")
	infra := &unstructured.Unstructured{}
	infra.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "config.openshift.io", Version: "v1", Kind: "Infrastructure",
	})
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "cluster"}, infra)).To(Succeed())
	name, found, err := unstructured.NestedString(infra.Object, "status", "infrastructureName")
	Expect(err).NotTo(HaveOccurred())
	Expect(found).To(BeTrue())
	clusterID = name

	// Start from a known state, and only here: there is deliberately no
	// AfterSuite.
	//
	// Ginkgo skips the remaining specs in an Ordered container once one
	// fails, so the deletion spec does not run and whatever broke is
	// left standing to be read. An AfterSuite would destroy exactly
	// that. Cleaning up front still makes the suite re-runnable, and
	// covers the case an AfterSuite cannot reach at all: a run killed
	// by Ctrl-C, by go test's timeout, or by a panic never gets to one.
	By("removing anything a previous run left behind")
	cleanupE2EObjects(ctx)
})

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

// cloudRouter reads the Cloud Router the profile names.
func cloudRouter(ctx context.Context) (*compute.Router, error) {
	return computeSvc.Routers.Get(
		bgpConfig.Spec.GCP.Project, bgpConfig.Spec.GCP.Region,
		bgpConfig.Spec.GCP.CloudRouterName).Context(ctx).Do()
}

// managedPeers are the BGP peers on the Cloud Router that belong to this
// cluster. Ownership is by name: a Cloud Router peer carries no labels,
// so the operator's own naming is the only mark, and PeerName is used
// rather than reimplemented so the two cannot drift.
func managedPeers(ctx context.Context) ([]*compute.RouterBgpPeer, error) {
	router, err := cloudRouter(ctx)
	if err != nil {
		return nil, err
	}
	prefix := gcpplatform.PeerName(clusterID, "0.0.0.0", 0)
	// PeerName appends the address and index, so trim those back off to
	// get the prefix the operator marks its peers with.
	prefix = strings.TrimSuffix(prefix, "-0-0-0-0-0")
	var out []*compute.RouterBgpPeer
	for _, p := range router.BgpPeers {
		if strings.HasPrefix(p.Name, prefix) {
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

// instanceFor reads the GCE instance behind a node, which is where
// canIpForward and nested virtualisation live.
func instanceFor(ctx context.Context, node *corev1.Node) (*compute.Instance, error) {
	vm, err := gcpplatform.ParseProviderID(node.Spec.ProviderID)
	if err != nil {
		return nil, err
	}
	return computeSvc.Instances.Get(vm.Project, vm.Zone, vm.Name).Context(ctx).Do()
}

// hubSpokes lists the router appliance spokes attached to the profile's
// hub, which is the resource Azure has no analogue for and which must be
// ACTIVE before anything can peer.
func hubSpokes(ctx context.Context) ([]*networkconnectivity.Spoke, error) {
	parent := "projects/" + bgpConfig.Spec.GCP.Project + "/locations/-"
	resp, err := nccSvc.Projects.Locations.Spokes.List(parent).Context(ctx).Do()
	if err != nil {
		return nil, err
	}
	var out []*networkconnectivity.Spoke
	for _, s := range resp.Spokes {
		if strings.Contains(s.Hub, bgpConfig.Spec.GCP.NCC.HubName) {
			out = append(out, s)
		}
	}
	return out, nil
}

// cleanupE2EObjects removes everything the suite creates and treats "not
// there" as success, so it is safe on a clean cluster and safe twice.
//
// The order is the one the finalizers require: the configuration refuses
// to go while a routing CR exists, so routing goes first. The operator
// has to be running for either finalizer to clear, which is why this
// never force-removes one -- a cleanup that stripped finalizers would
// leave the spoke and the peers behind and hide the fact that the
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

// ReportAfterEach prints everything worth having when a spec fails.
//
// In CI this is the only record that survives: hack/ci-e2e-gcp.sh tears
// the estate down whatever the result, which deletes the configuration
// and takes the spoke and peers with it, so by the time prow gathers
// artefacts there is nothing left to read. It is deliberately everything
// at once, because a run costs the best part of two hours and a
// diagnostic that sends you round again to ask the next question is
// worth very little.
//
// Nothing here asserts, and it tolerates the clients being nil, because
// a failure inside a reporting node would replace the failure you are
// trying to read.
var _ = ReportAfterEach(func(report SpecReport) {
	if !report.Failed() {
		return
	}
	dumpEverything(report.LeafNodeText)
})

// ReportAfterSuite covers what ReportAfterEach cannot: a failure in
// BeforeSuite runs no spec at all, and the start-of-run cleanup waiting
// out a finalizer is exactly the kind of thing that would fail there and
// say nothing.
var _ = ReportAfterSuite("diagnostics", func(report Report) {
	if report.SuiteSucceeded {
		return
	}
	for _, spec := range report.SpecReports {
		if spec.State.Is(ginkgotypes.SpecStatePassed | ginkgotypes.SpecStateFailed) {
			return
		}
	}
	dumpEverything("suite setup")
})

func say(format string, args ...any) {
	GinkgoWriter.Printf(format+"\n", args...)
}

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
	dumpGCP(ctx)
	dumpWarnings(ctx)
	dumpOperatorLog(ctx)
	say("================ end diagnostics ================")
}

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
}

// dumpCredentials is the first fork in the road: no request at all means
// the operator failed before it resolved a credential, which is a very
// different problem from one it resolved and could not use.
func dumpCredentials(ctx context.Context) {
	say("--- credentials ---")
	cr := &unstructured.Unstructured{}
	cr.SetGroupVersionKind(gcpplatform.CredentialsRequestGVK)
	err := k8sClient.Get(ctx, types.NamespacedName{
		Name: gcpplatform.CredentialsRequestName, Namespace: gcpplatform.CredentialsRequestNamespace,
	}, cr)
	if err != nil {
		say("  CredentialsRequest: %v", err)
	} else {
		provisioned, _, _ := unstructured.NestedBool(cr.Object, "status", "provisioned")
		perms, _, _ := unstructured.NestedStringSlice(cr.Object, "spec", "providerSpec", "permissions")
		say("  CredentialsRequest: provisioned=%v, %d permissions", provisioned, len(perms))
	}
	secret, err := clientset.CoreV1().Secrets(operatorNamespace).
		Get(ctx, gcpplatform.CredentialsSecretName, metav1.GetOptions{})
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

// dumpGCP is what the cloud says, as opposed to what the cluster
// believes. The two disagreeing is the whole point of an e2e.
func dumpGCP(ctx context.Context) {
	say("--- GCP ---")
	router, err := cloudRouter(ctx)
	if err != nil {
		say("  cannot read Cloud Router %s: %v", bgpConfig.Spec.GCP.CloudRouterName, err)
	} else {
		say("  Cloud Router %s asn=%d", router.Name, router.Bgp.Asn)
		for _, i := range router.Interfaces {
			say("    interface %s %s redundant=%s", i.Name, i.IpRange, i.RedundantInterface)
		}
		for _, p := range router.BgpPeers {
			say("    peer %s -> %s asn=%d iface=%s enable=%s",
				p.Name, p.PeerIpAddress, p.PeerAsn, p.InterfaceName, p.Enable)
		}
	}

	spokes, err := hubSpokes(ctx)
	if err != nil {
		say("  cannot list spokes: %v", err)
	} else {
		say("  %d spoke(s) on hub %s", len(spokes), bgpConfig.Spec.GCP.NCC.HubName)
		for _, s := range spokes {
			say("    %s state=%s", s.Name, s.State)
		}
	}

	nodes, nodeErr := routerNodes(ctx)
	if nodeErr != nil {
		return
	}
	for i := range nodes {
		inst, instErr := instanceFor(ctx, &nodes[i])
		if instErr != nil {
			say("    %s: cannot read instance: %v", nodes[i].Name, instErr)
			continue
		}
		forward := false
		for _, nic := range inst.NetworkInterfaces {
			_ = nic
		}
		forward = inst.CanIpForward
		say("    %s: canIpForward=%v", nodes[i].Name, forward)
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
