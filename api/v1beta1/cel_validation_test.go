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

package v1beta1_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

var (
	testEnv    *envtest.Environment
	testClient client.Client
	cfg        *rest.Config
	ctx        = context.Background()
)

func TestMain(m *testing.M) {
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
		},
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		panic(err)
	}

	s := runtime.NewScheme()
	if err := networkingapi.AddToScheme(s); err != nil {
		panic(err)
	}

	testClient, err = client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		panic(err)
	}

	code := m.Run()
	if err := testEnv.Stop(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to stop test environment: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func objName(t *testing.T) string {
	t.Helper()
	return strings.ToLower(strings.NewReplacer("_", "-", "/", "-").Replace(t.Name()))
}

// validManualConfig returns a minimal valid BGPCloudConfiguration with platform Manual.
func validManualConfig(name string) *networkingapi.BGPCloudConfiguration {
	return &networkingapi.BGPCloudConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: networkingapi.BGPCloudConfigurationSpec{
			Platform: networkingapi.PlatformManual,
			BGP: networkingapi.BGPConfig{
				LocalASN: 65001,
				PeerGroups: []networkingapi.PeerGroup{{
					NodeSelector: map[string]string{"zone": "a"},
					Neighbors: []networkingapi.BGPNeighbor{{
						Address:   "10.0.0.1",
						RemoteASN: 64512,
					}},
				}},
			},
			RouterNodeSelector: map[string]string{"role": "router"},
		},
	}
}

// --- BGPCloudConfiguration spec-level CEL validation tests ---

func TestCEL_ManualValid(t *testing.T) {
	obj := validManualConfig(objName(t))
	if err := testClient.Create(ctx, obj); err != nil {
		t.Fatalf("valid Manual config should be accepted: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
}

func TestCEL_ManualRequiresPeerGroups(t *testing.T) {
	obj := validManualConfig(objName(t))
	obj.Spec.BGP.PeerGroups = nil

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("Manual without peerGroups should be rejected")
	}
	assertErrorContains(t, err, "spec.bgp.peerGroups is required when spec.platform is Manual")
}

func TestCEL_ManualEmptyPeerGroups(t *testing.T) {
	obj := validManualConfig(objName(t))
	obj.Spec.BGP.PeerGroups = []networkingapi.PeerGroup{}

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("Manual with empty peerGroups should be rejected")
	}
	assertErrorContains(t, err, "spec.bgp.peerGroups is required when spec.platform is Manual")
}

func TestCEL_AWSRequiresAWSBlock(t *testing.T) {
	obj := validManualConfig(objName(t))
	obj.Spec.Platform = networkingapi.PlatformAWS
	obj.Spec.BGP.PeerGroups = nil

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("AWS without spec.aws should be rejected")
	}
	assertErrorContains(t, err, "spec.aws must be set when spec.platform is AWS")
}

func TestCEL_AWSBlockOnNonAWS(t *testing.T) {
	obj := validManualConfig(objName(t))
	obj.Spec.AWS = &networkingapi.AWSConfig{
		Region:         "us-east-1",
		RouteServerIDs: []string{"rs-123"},
	}

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("Manual with spec.aws should be rejected")
	}
	assertErrorContains(t, err, "spec.aws must be set when spec.platform is AWS")
}

func TestCEL_AWSValid(t *testing.T) {
	obj := &networkingapi.BGPCloudConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: objName(t)},
		Spec: networkingapi.BGPCloudConfigurationSpec{
			Platform: networkingapi.PlatformAWS,
			BGP:      networkingapi.BGPConfig{LocalASN: 65001},
			AWS: &networkingapi.AWSConfig{
				Region:         "us-east-1",
				RouteServerIDs: []string{"rs-123"},
			},
			RouterNodeSelector: map[string]string{"role": "router"},
		},
	}

	if err := testClient.Create(ctx, obj); err != nil {
		t.Fatalf("valid AWS config should be accepted: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
}

func TestCEL_AzureRequiresAzureBlock(t *testing.T) {
	obj := validManualConfig(objName(t))
	obj.Spec.Platform = networkingapi.PlatformAzure
	obj.Spec.BGP.PeerGroups = nil

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("Azure without spec.azure should be rejected")
	}
	assertErrorContains(t, err, "spec.azure must be set when spec.platform is Azure")
}

func TestCEL_AzureValid(t *testing.T) {
	obj := &networkingapi.BGPCloudConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: objName(t)},
		Spec: networkingapi.BGPCloudConfigurationSpec{
			Platform: networkingapi.PlatformAzure,
			BGP:      networkingapi.BGPConfig{LocalASN: 65001},
			Azure: &networkingapi.AzureConfig{
				SubscriptionID:  "sub-123",
				ResourceGroup:   "rg-1",
				RouteServerName: "rs-1",
			},
			RouterNodeSelector: map[string]string{"role": "router"},
		},
	}

	if err := testClient.Create(ctx, obj); err != nil {
		t.Fatalf("valid Azure config should be accepted: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
}

func TestCEL_GCPRequiresGCPBlock(t *testing.T) {
	obj := validManualConfig(objName(t))
	obj.Spec.Platform = networkingapi.PlatformGCP
	obj.Spec.BGP.PeerGroups = nil

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("GCP without spec.gcp should be rejected")
	}
	assertErrorContains(t, err, "spec.gcp must be set when spec.platform is GCP")
}

func TestCEL_GCPValid(t *testing.T) {
	nested := true
	obj := &networkingapi.BGPCloudConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: objName(t)},
		Spec: networkingapi.BGPCloudConfigurationSpec{
			Platform: networkingapi.PlatformGCP,
			BGP:      networkingapi.BGPConfig{LocalASN: 65001},
			GCP: &networkingapi.GCPConfig{
				Project:         "my-proj",
				Region:          "us-central1",
				CloudRouterName: "my-router",
				NCC: networkingapi.NCCConfig{
					HubName:     "hub-1",
					SpokePrefix: "spoke",
				},
				EnableNestedVirtualization: &nested,
			},
			RouterNodeSelector: map[string]string{"role": "router"},
		},
	}

	if err := testClient.Create(ctx, obj); err != nil {
		t.Fatalf("valid GCP config should be accepted: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
}

func TestCEL_NonManualRejectsPeerGroups(t *testing.T) {
	obj := &networkingapi.BGPCloudConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: objName(t)},
		Spec: networkingapi.BGPCloudConfigurationSpec{
			Platform: networkingapi.PlatformAWS,
			BGP: networkingapi.BGPConfig{
				LocalASN: 65001,
				PeerGroups: []networkingapi.PeerGroup{{
					NodeSelector: map[string]string{"zone": "a"},
					Neighbors:    []networkingapi.BGPNeighbor{{Address: "10.0.0.1", RemoteASN: 64512}},
				}},
			},
			AWS: &networkingapi.AWSConfig{
				Region:         "us-east-1",
				RouteServerIDs: []string{"rs-123"},
			},
			RouterNodeSelector: map[string]string{"role": "router"},
		},
	}

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("non-Manual with peerGroups should be rejected")
	}
	assertErrorContains(t, err, "spec.bgp.peerGroups may only be set when spec.platform is Manual")
}

func TestCEL_GCPBlockOnNonGCP(t *testing.T) {
	obj := validManualConfig(objName(t))
	nested := true
	obj.Spec.GCP = &networkingapi.GCPConfig{
		Project: "p", Region: "r", CloudRouterName: "cr",
		NCC:                        networkingapi.NCCConfig{HubName: "h", SpokePrefix: "s"},
		EnableNestedVirtualization: &nested,
	}

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("Manual with spec.gcp should be rejected")
	}
	assertErrorContains(t, err, "spec.gcp must be set when spec.platform is GCP")
}

func TestCEL_AzureBlockOnNonAzure(t *testing.T) {
	obj := validManualConfig(objName(t))
	obj.Spec.Azure = &networkingapi.AzureConfig{
		SubscriptionID: "sub", ResourceGroup: "rg", RouteServerName: "rs",
	}

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("Manual with spec.azure should be rejected")
	}
	assertErrorContains(t, err, "spec.azure must be set when spec.platform is Azure")
}

func TestCEL_EmptyRouterNodeSelector(t *testing.T) {
	obj := validManualConfig(objName(t))
	obj.Spec.RouterNodeSelector = map[string]string{}

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("empty routerNodeSelector should be rejected")
	}
	assertErrorContains(t, err, "routerNodeSelector must not be empty")
}

func TestCEL_DuplicateNeighborAddresses(t *testing.T) {
	obj := validManualConfig(objName(t))
	obj.Spec.BGP.PeerGroups[0].Neighbors = []networkingapi.BGPNeighbor{
		{Address: "10.0.0.1", RemoteASN: 64512},
		{Address: "10.0.0.1", RemoteASN: 64513},
	}

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("duplicate neighbor addresses should be rejected")
	}
	assertErrorContains(t, err, "duplicate neighbor addresses within a peer group are not allowed")
}

func TestCEL_DuplicateNeighborEquivalentIPv6(t *testing.T) {
	obj := validManualConfig(objName(t))
	obj.Spec.BGP.PeerGroups[0].Neighbors = []networkingapi.BGPNeighbor{
		{Address: "2001:db8::1", RemoteASN: 64512},
		{Address: "2001:0db8:0000:0000:0000:0000:0000:0001", RemoteASN: 64513},
	}

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("equivalent IPv6 neighbor addresses should be rejected")
	}
	assertErrorContains(t, err, "duplicate neighbor addresses within a peer group are not allowed")
}

func TestCEL_DuplicateRouteServerIDs(t *testing.T) {
	obj := &networkingapi.BGPCloudConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: objName(t)},
		Spec: networkingapi.BGPCloudConfigurationSpec{
			Platform: networkingapi.PlatformAWS,
			BGP:      networkingapi.BGPConfig{LocalASN: 65001},
			AWS: &networkingapi.AWSConfig{
				Region:         "us-east-1",
				RouteServerIDs: []string{"rs-123", "rs-123"},
			},
			RouterNodeSelector: map[string]string{"role": "router"},
		},
	}

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("duplicate route server IDs should be rejected")
	}
	assertErrorContains(t, err, "duplicate route server IDs are not allowed")
}

func TestCEL_PlatformImmutable(t *testing.T) {
	obj := validManualConfig(objName(t))
	if err := testClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })

	fetched := &networkingapi.BGPCloudConfiguration{}
	if err := testClient.Get(ctx, client.ObjectKey{Name: objName(t)}, fetched); err != nil {
		t.Fatalf("get: %v", err)
	}
	fetched.Spec.Platform = networkingapi.PlatformAWS
	fetched.Spec.BGP.PeerGroups = nil
	fetched.Spec.AWS = &networkingapi.AWSConfig{
		Region:         "us-east-1",
		RouteServerIDs: []string{"rs-123"},
	}

	err := testClient.Update(ctx, fetched)
	if err == nil {
		t.Fatal("changing spec.platform after creation should be rejected")
	}
	assertErrorContains(t, err, "spec.platform cannot be changed after creation")
}

func TestCEL_PlatformUpdateSamePlatformAllowed(t *testing.T) {
	obj := validManualConfig(objName(t))
	if err := testClient.Create(ctx, obj); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })

	fetched := &networkingapi.BGPCloudConfiguration{}
	if err := testClient.Get(ctx, client.ObjectKey{Name: objName(t)}, fetched); err != nil {
		t.Fatalf("get: %v", err)
	}
	fetched.Spec.BGP.LocalASN = 65002
	if err := testClient.Update(ctx, fetched); err != nil {
		t.Fatalf("same-platform update should be accepted: %v", err)
	}
}

// --- Field-level CEL: isIP and isCIDR ---

func TestCEL_InvalidNeighborIP(t *testing.T) {
	obj := validManualConfig(objName(t))
	obj.Spec.BGP.PeerGroups[0].Neighbors[0].Address = "not-an-ip"

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("invalid neighbor IP should be rejected")
	}
	assertErrorContains(t, err, "must be a valid IP address")
}

func TestCEL_ValidIPv6Neighbor(t *testing.T) {
	obj := validManualConfig(objName(t))
	obj.Spec.BGP.PeerGroups[0].Neighbors[0].Address = "2001:db8::1"

	if err := testClient.Create(ctx, obj); err != nil {
		t.Fatalf("valid IPv6 neighbor should be accepted: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
}

// --- BGPRouting validation tests ---

func validRouting(name string) *networkingapi.BGPRouting {
	return &networkingapi.BGPRouting{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: networkingapi.BGPRoutingSpec{
			Network: networkingapi.NetworkConfig{
				Name:    "prod",
				Subnets: []string{"10.100.0.0/16"},
			},
		},
	}
}

func TestCEL_RoutingValid(t *testing.T) {
	obj := validRouting("cel-routing-valid")
	if err := testClient.Create(ctx, obj); err != nil {
		t.Fatalf("valid BGPRouting should be accepted: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
}

func TestCEL_RoutingDualStack(t *testing.T) {
	obj := validRouting("cel-routing-dual")
	obj.Spec.Network.Subnets = []string{"10.100.0.0/16", "2001:db8::/64"}

	if err := testClient.Create(ctx, obj); err != nil {
		t.Fatalf("dual-stack BGPRouting should be accepted: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
}

func TestCEL_RoutingInvalidCIDR(t *testing.T) {
	obj := validRouting("cel-routing-bad-cidr")
	obj.Spec.Network.Subnets = []string{"not-a-cidr"}

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("invalid CIDR should be rejected")
	}
	assertErrorContains(t, err, "each subnet must be a valid CIDR")
}

func TestCEL_RoutingBareIP(t *testing.T) {
	obj := validRouting("cel-routing-bare-ip")
	obj.Spec.Network.Subnets = []string{"10.0.0.1"}

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("bare IP (not CIDR) should be rejected")
	}
	assertErrorContains(t, err, "each subnet must be a valid CIDR")
}

func TestCEL_RoutingInvalidNetworkName(t *testing.T) {
	obj := validRouting("cel-routing-bad-name")
	obj.Spec.Network.Name = "Prod_Net"

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("non DNS-1123 network name should be rejected")
	}
}

func TestCEL_RoutingNetworkNameTooLong(t *testing.T) {
	obj := validRouting("cel-routing-long-name")
	obj.Spec.Network.Name = strings.Repeat("a", 64)

	err := testClient.Create(ctx, obj)
	if err == nil {
		t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })
		t.Fatal("network name longer than 63 characters should be rejected")
	}
}

// --- helpers ---

func assertErrorContains(t *testing.T, err error, substr string) {
	t.Helper()
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("expected error to contain %q, got:\n%s", substr, err.Error())
	}
}
