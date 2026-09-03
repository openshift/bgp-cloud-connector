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
	e2e "github.com/openshift/bgp-cloud-connector/test/e2e"
)

var (
	k8sClient client.Client
	ec2Client *ec2.Client

	bgpConfig  *networkingv1alpha1.CUDNBgpConfig
	bgpRouting *networkingv1alpha1.CUDNBgpRouting

	clusterID     string
	managedByTag  string
	endpointsByAZ map[string][]string
)

func TestAWSE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "AWS E2E Suite")
}

var _ = BeforeSuite(func() {
	// E2E_MANIFEST_DIR takes an absolute path and skips the profile
	// lookup entirely. A profile has to be a directory under
	// test/e2e/manifests, which is fine for one checked in by hand and
	// wrong for one generated per run: the route server id is minted
	// while the job is running, so the manifest cannot be written until
	// then, and it has no business being written into the source tree.
	manifestDir := os.Getenv("E2E_MANIFEST_DIR")
	profile := os.Getenv("E2E_PROFILE")
	if manifestDir == "" {
		Expect(profile).NotTo(BeEmpty(),
			"set E2E_PROFILE (e.g. make test-e2e-aws my-cluster) or E2E_MANIFEST_DIR")
		manifestDir = filepath.Join("..", "..", "..", "test", "e2e", "manifests", profile)
	}

	By("loading CUDNBgpConfig manifest from " + manifestDir)
	bgpConfig = &networkingv1alpha1.CUDNBgpConfig{}
	loadManifest(filepath.Join(manifestDir, "cudnbgpconfig.yaml"), bgpConfig)
	Expect(bgpConfig.Spec.AWS).NotTo(BeNil(), "profile CUDNBgpConfig must have spec.aws")

	By("loading CUDNBgpRouting manifest from " + manifestDir)
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
		e2e.NetworkOperatorGVK,
	} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
}

// listManagedPeers returns the peers this operator owns on an endpoint, and
// only those that still exist.
//
// EC2 goes on returning a peer after it has been deleted, so filtering on the
// managed-by tag alone counts corpses. That cuts both ways: it makes "the
// peers were deleted" fail on peers that were, and it makes "peers exist per
// AZ" pass on peers that do not.
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
		if peer.State == ec2types.RouteServerPeerStateDeleted {
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
