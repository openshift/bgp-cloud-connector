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

package aws_e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1alpha1 "github.com/openshift/bgp-cloud-connector/api/v1alpha1"
)

var (
	k8sClient client.Client
	ec2Client *ec2.Client

	bgpConfig  *networkingv1alpha1.CUDNBgpConfig
	bgpRouting *networkingv1alpha1.CUDNBgpRouting

	clusterID    string
	managedByTag string
	// testNamespace is generated at run time, so cleanup must use the
	// name the API server actually assigned.
	testNamespace string
	endpointsByAZ map[string][]string
)

func TestAWSE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "AWS E2E Suite")
}

var _ = BeforeSuite(func() {
	profile := os.Getenv("E2E_PROFILE")
	Expect(profile).NotTo(BeEmpty(), "E2E_PROFILE must be set (e.g. make test-e2e-aws my-cluster)")
	manifestDir := filepath.Join("..", "..", "..", "test", "e2e", "manifests", profile)

	By("loading CUDNBgpConfig manifest from profile " + profile)
	bgpConfig = &networkingv1alpha1.CUDNBgpConfig{}
	loadManifest(filepath.Join(manifestDir, "cudnbgpconfig.yaml"), bgpConfig)
	Expect(bgpConfig.Spec.AWS).NotTo(BeNil(), "profile CUDNBgpConfig must have spec.aws")

	By("loading CUDNBgpRouting manifest from profile " + profile)
	bgpRouting = &networkingv1alpha1.CUDNBgpRouting{}
	loadManifest(filepath.Join(manifestDir, "cudnbgprouting.yaml"), bgpRouting)

	By("building kubernetes client")
	scheme := runtime.NewScheme()
	Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	Expect(networkingv1alpha1.AddToScheme(scheme)).To(Succeed())
	addUnstructuredTypes(scheme)

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{},
	).ClientConfig()
	Expect(err).NotTo(HaveOccurred())
	k8sClient, err = client.New(restCfg, client.Options{Scheme: scheme})
	Expect(err).NotTo(HaveOccurred())

	By("building AWS client using default credential chain")
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(bgpConfig.Spec.AWS.Region),
	)
	Expect(err).NotTo(HaveOccurred())
	ec2Client = ec2.NewFromConfig(awsCfg)

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
	managedByTag = "cudn-bgp-routing-operator/" + clusterID

	By("discovering route server endpoints from AWS")
	endpointsByAZ = make(map[string][]string)
	for _, rsID := range bgpConfig.Spec.AWS.RouteServerIDs {
		rseOut, rseErr := ec2Client.DescribeRouteServerEndpoints(
			context.Background(), &ec2.DescribeRouteServerEndpointsInput{})
		Expect(rseErr).NotTo(HaveOccurred())
		subnetIDs := make([]string, 0)
		epBySubnet := make(map[string][]string)
		for _, ep := range rseOut.RouteServerEndpoints {
			if aws.ToString(ep.RouteServerId) != rsID {
				continue
			}
			sid := aws.ToString(ep.SubnetId)
			epBySubnet[sid] = append(epBySubnet[sid], aws.ToString(ep.RouteServerEndpointId))
			subnetIDs = append(subnetIDs, sid)
		}
		subOut, subErr := ec2Client.DescribeSubnets(context.Background(), &ec2.DescribeSubnetsInput{SubnetIds: subnetIDs})
		Expect(subErr).NotTo(HaveOccurred())
		for _, s := range subOut.Subnets {
			az := aws.ToString(s.AvailabilityZone)
			sid := aws.ToString(s.SubnetId)
			endpointsByAZ[az] = append(endpointsByAZ[az], epBySubnet[sid]...)
		}
	}
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

// The suite creates a CUDNBgpConfig, a CUDNBgpRouting and a namespace, and
// Create is not idempotent: anything a failed run leaves behind makes the
// next run fail on AlreadyExists, needing manual cleanup before you can try
// again. AfterSuite runs whether or not specs passed, so the suite becomes
// re-runnable.
//
// Order matters. The config CR has a finalizer that blocks its deletion
// while any routing CR exists, which E2E-AWS-05 asserts deliberately, so
// routing goes first.
//
// Set E2E_SKIP_CLEANUP to keep the wreckage for a post-mortem.
var _ = AfterSuite(func(ctx SpecContext) {
	if os.Getenv("E2E_SKIP_CLEANUP") != "" {
		GinkgoWriter.Println("E2E_SKIP_CLEANUP set; leaving resources in place")
		return
	}
	if k8sClient == nil {
		return
	}

	deleteAndWait := func(obj client.Object, what string) {
		if err := k8sClient.Delete(ctx, obj); err != nil {
			if !apierrors.IsNotFound(err) {
				GinkgoWriter.Printf("cleanup: deleting %s: %v\n", what, err)
			}
			return
		}
		GinkgoWriter.Printf("cleanup: deleted %s\n", what)
		Eventually(func() bool {
			err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), obj)
			return apierrors.IsNotFound(err)
		}, 3*time.Minute, 5*time.Second).Should(BeTrue(), "%s should go away", what)
	}

	if bgpRouting != nil {
		r := &networkingv1alpha1.CUDNBgpRouting{}
		r.Name = bgpRouting.Name
		deleteAndWait(r, "CUDNBgpRouting/"+bgpRouting.Name)
	}

	if bgpConfig != nil {
		c := &networkingv1alpha1.CUDNBgpConfig{}
		c.Name = bgpConfig.Name
		deleteAndWait(c, "CUDNBgpConfig/"+bgpConfig.Name)
	}

	if testNamespace != "" {
		ns := &corev1.Namespace{}
		ns.Name = testNamespace
		deleteAndWait(ns, "namespace/"+ns.Name)
	}
})

// createEventually retries Create while the object is still going away.
//
// Both CRs have fixed names, and the config CR carries a finalizer that is
// only removed on the next reconcile, so it lingers in Terminating for up
// to a resync interval after AfterSuite deletes it. A run started in that
// window used to fail immediately with "object is being deleted", which is
// exactly what happens when you interrupt a run and start another.
func createEventually(ctx context.Context, obj client.Object, what string) {
	GinkgoHelper()
	Eventually(func() error {
		err := k8sClient.Create(ctx, obj)
		if apierrors.IsAlreadyExists(err) {
			GinkgoWriter.Printf("waiting for previous %s to finish deleting\n", what)
		}
		return err
	}, 3*time.Minute, 5*time.Second).Should(Succeed(), "creating %s", what)
}

func listManagedPeers(ctx context.Context, endpointID string) ([]ec2types.RouteServerPeer, error) {
	out, err := ec2Client.DescribeRouteServerPeers(ctx, &ec2.DescribeRouteServerPeersInput{})
	if err != nil {
		return nil, err
	}
	var filtered []ec2types.RouteServerPeer
	for _, peer := range out.RouteServerPeers {
		if aws.ToString(peer.RouteServerEndpointId) != endpointID {
			continue
		}
		if !peerIsAlive(peer.State) {
			continue
		}
		for _, t := range peer.Tags {
			if aws.ToString(t.Key) == "managed-by" && aws.ToString(t.Value) == managedByTag {
				filtered = append(filtered, peer)
				break
			}
		}
	}
	return filtered, nil
}

// peerIsAlive mirrors the operator's own check. DescribeRouteServerPeers
// keeps returning peers after they are gone, so without this the suite
// tries to delete peers AWS has already deleted, which fails with
// IncorrectState, and counts dead peers when asserting cleanup.
func peerIsAlive(state ec2types.RouteServerPeerState) bool {
	switch state {
	case ec2types.RouteServerPeerStateAvailable, ec2types.RouteServerPeerStatePending:
		return true
	default:
		return false
	}
}

func allManagedPeers(ctx context.Context) ([]ec2types.RouteServerPeer, error) {
	var all []ec2types.RouteServerPeer
	for _, eps := range endpointsByAZ {
		for _, ep := range eps {
			peers, err := listManagedPeers(ctx, ep)
			if err != nil {
				return nil, err
			}
			all = append(all, peers...)
		}
	}
	return all, nil
}

func routerNodes(ctx context.Context) ([]corev1.Node, error) {
	nodeList := &corev1.NodeList{}
	if err := k8sClient.List(ctx, nodeList, client.MatchingLabels(bgpConfig.Spec.RouterNodeSelector)); err != nil {
		return nil, err
	}
	var result []corev1.Node
	for _, n := range nodeList.Items {
		ip := nodeInternalIP(&n)
		az := n.Labels["topology.kubernetes.io/zone"]
		if ip != "" && az != "" && n.Spec.ProviderID != "" {
			result = append(result, n)
		}
	}
	return result, nil
}

func nodeInternalIP(node *corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}

func describeInstance(ctx context.Context, instanceID string) (*ec2types.Instance, error) {
	out, err := ec2Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	})
	if err != nil {
		return nil, err
	}
	for _, res := range out.Reservations {
		for i := range res.Instances {
			return &res.Instances[i], nil
		}
	}
	return nil, fmt.Errorf("instance %s not found", instanceID)
}

func primaryENI(inst *ec2types.Instance) *ec2types.InstanceNetworkInterface {
	for i := range inst.NetworkInterfaces {
		if inst.NetworkInterfaces[i].Attachment != nil &&
			aws.ToInt32(inst.NetworkInterfaces[i].Attachment.DeviceIndex) == 0 {
			return &inst.NetworkInterfaces[i]
		}
	}
	return nil
}
