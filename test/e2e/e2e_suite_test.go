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

package e2e

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

const (
	frrNamespace        = "openshift-frr-k8s"
	frrConfigNamePrefix = "bgp-cc-"
	labelManagedBy      = "app.kubernetes.io/managed-by"
	labelManagedByVal   = "bgp-cloud-connector"
	labelPrimaryUDN     = "k8s.ovn.org/primary-user-defined-network"
	labelClusterUDN     = "cluster-udn"
	raName              = "bgp-cc-route-advertisements"
)

var (
	k8sClient  client.Client
	bgpConfig  *networkingapi.BGPCloudConfiguration
	bgpRouting *networkingapi.BGPRouting
)

var (
	bgpSessionStateGVK = schema.GroupVersionKind{
		Group: "frrk8s.metallb.io", Version: "v1beta1", Kind: "BGPSessionState",
	}
	frrConfigurationGVK = schema.GroupVersionKind{
		Group: "frrk8s.metallb.io", Version: "v1beta1", Kind: "FRRConfiguration",
	}
	clusterUDNGVK = schema.GroupVersionKind{
		Group: "k8s.ovn.org", Version: "v1", Kind: "ClusterUserDefinedNetwork",
	}
	raGVK = schema.GroupVersionKind{
		Group: "k8s.ovn.org", Version: "v1", Kind: "RouteAdvertisements",
	}
	frrNodeStateGVK = schema.GroupVersionKind{
		Group: "frrk8s.metallb.io", Version: "v1beta1", Kind: "FRRNodeState",
	}
)

func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E Suite")
}

var _ = BeforeSuite(func() {
	// See the note in the AWS suite: a generated profile cannot live
	// under test/e2e/manifests, so an explicit directory wins.
	manifestDir := os.Getenv("E2E_MANIFEST_DIR")
	profile := os.Getenv("E2E_PROFILE")
	if manifestDir == "" {
		Expect(profile).NotTo(BeEmpty(),
			"set E2E_PROFILE (e.g. make test-e2e my-cluster) or E2E_MANIFEST_DIR")
		manifestDir = filepath.Join("..", "..", "test", "e2e", "manifests", profile)
	}

	By("loading BGPCloudConfiguration manifest from " + manifestDir)
	bgpConfig = &networkingapi.BGPCloudConfiguration{}
	loadManifest(filepath.Join(manifestDir, "bgpcloudconfiguration.yaml"), bgpConfig)
	Expect(bgpConfig.Spec.AWS).To(BeNil(), "shared E2E profile must not have spec.aws")
	Expect(bgpConfig.Spec.BGP.PeerGroups).NotTo(BeEmpty(),
		"shared E2E profile must have spec.bgp.peerGroups")

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

	By("checking for pre-existing state that would conflict with tests")
	ctx := context.Background()
	config := &networkingapi.BGPCloudConfiguration{}
	Expect(client.IgnoreNotFound(
		k8sClient.Get(ctx, types.NamespacedName{Name: bgpConfig.Name}, config),
	)).To(Succeed())
	Expect(config.Name).To(BeEmpty(),
		"BGPCloudConfiguration %q already exists — delete it before running E2E tests", bgpConfig.Name)

	routing := &networkingapi.BGPRouting{}
	Expect(client.IgnoreNotFound(
		k8sClient.Get(ctx, types.NamespacedName{Name: bgpRouting.Name}, routing),
	)).To(Succeed())
	Expect(routing.Name).To(BeEmpty(),
		"BGPRouting %q already exists — delete it before running E2E tests", bgpRouting.Name)

	ns := &corev1.Namespace{}
	Expect(client.IgnoreNotFound(
		k8sClient.Get(ctx, types.NamespacedName{Name: "app1"}, ns),
	)).To(Succeed())
	Expect(ns.Name).To(BeEmpty(),
		"namespace app1 already exists — delete it before running E2E tests")
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
		frrConfigurationGVK,
		clusterUDNGVK,
		raGVK,
		bgpSessionStateGVK,
		frrNodeStateGVK,
	} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
}

func routerNodes(ctx context.Context) ([]corev1.Node, error) {
	nodeList := &corev1.NodeList{}
	if err := k8sClient.List(ctx, nodeList,
		client.MatchingLabels(bgpConfig.Spec.RouterNodeSelector)); err != nil {
		return nil, err
	}
	return nodeList.Items, nil
}
