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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

func TestRoundtrip_BGPCloudConfiguration(t *testing.T) {
	nested := true
	orig := &networkingapi.BGPCloudConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "rt-config"},
		Spec: networkingapi.BGPCloudConfigurationSpec{
			Platform: networkingapi.PlatformGCP,
			BGP: networkingapi.BGPConfig{
				LocalASN:          65001,
				LivenessDetection: networkingapi.LivenessDetectionBFD,
			},
			GCP: &networkingapi.GCPConfig{
				Project:         "my-project",
				Region:          "us-central1",
				CloudRouterName: "router-1",
				NCC: networkingapi.NCCConfig{
					HubName:                "hub",
					SpokePrefix:            "spoke",
					SiteToSiteDataTransfer: true,
				},
				EnableNestedVirtualization: &nested,
			},
			RouterNodeSelector: map[string]string{"role": "router", "zone": "a"},
		},
	}

	if err := testClient.Create(ctx, orig); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, orig) })

	fetched := &networkingapi.BGPCloudConfiguration{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: "rt-config"}, fetched); err != nil {
		t.Fatalf("get: %v", err)
	}

	if fetched.Spec.Platform != orig.Spec.Platform {
		t.Errorf("platform = %v, want %v", fetched.Spec.Platform, orig.Spec.Platform)
	}
	if fetched.Spec.BGP.LocalASN != orig.Spec.BGP.LocalASN {
		t.Errorf("localASN = %d, want %d", fetched.Spec.BGP.LocalASN, orig.Spec.BGP.LocalASN)
	}
	if fetched.Spec.BGP.LivenessDetection != orig.Spec.BGP.LivenessDetection {
		t.Errorf("livenessDetection = %v, want %v", fetched.Spec.BGP.LivenessDetection, orig.Spec.BGP.LivenessDetection)
	}
	if fetched.Spec.GCP == nil {
		t.Fatal("spec.gcp is nil after roundtrip")
	}
	if fetched.Spec.GCP.Project != "my-project" {
		t.Errorf("gcp.project = %q, want %q", fetched.Spec.GCP.Project, "my-project")
	}
	if fetched.Spec.GCP.CloudRouterName != "router-1" {
		t.Errorf("gcp.cloudRouterName = %q, want %q", fetched.Spec.GCP.CloudRouterName, "router-1")
	}
	if fetched.Spec.GCP.NCC.HubName != "hub" {
		t.Errorf("ncc.hubName = %q, want %q", fetched.Spec.GCP.NCC.HubName, "hub")
	}
	if !fetched.Spec.GCP.NCC.SiteToSiteDataTransfer {
		t.Error("ncc.siteToSiteDataTransfer should be true")
	}
	if fetched.Spec.GCP.EnableNestedVirtualization == nil || !*fetched.Spec.GCP.EnableNestedVirtualization {
		t.Error("enableNestedVirtualization should be true")
	}
	if len(fetched.Spec.RouterNodeSelector) != 2 {
		t.Errorf("routerNodeSelector has %d entries, want 2", len(fetched.Spec.RouterNodeSelector))
	}
}

func TestRoundtrip_BGPRouting(t *testing.T) {
	orig := &networkingapi.BGPRouting{
		ObjectMeta: metav1.ObjectMeta{Name: "rt-routing"},
		Spec: networkingapi.BGPRoutingSpec{
			Network: networkingapi.NetworkConfig{
				Name:    "prod",
				Subnets: []string{"10.100.0.0/16", "2001:db8::/64"},
			},
		},
	}

	if err := testClient.Create(ctx, orig); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = testClient.Delete(ctx, orig) })

	fetched := &networkingapi.BGPRouting{}
	if err := testClient.Get(ctx, types.NamespacedName{Name: "rt-routing"}, fetched); err != nil {
		t.Fatalf("get: %v", err)
	}

	if fetched.Spec.Network.Name != "prod" {
		t.Errorf("network.name = %q, want %q", fetched.Spec.Network.Name, "prod")
	}
	if len(fetched.Spec.Network.Subnets) != 2 {
		t.Fatalf("subnets has %d entries, want 2", len(fetched.Spec.Network.Subnets))
	}
	if fetched.Spec.Network.Subnets[0] != "10.100.0.0/16" {
		t.Errorf("subnet[0] = %q, want %q", fetched.Spec.Network.Subnets[0], "10.100.0.0/16")
	}
	if fetched.Spec.Network.Subnets[1] != "2001:db8::/64" {
		t.Errorf("subnet[1] = %q, want %q", fetched.Spec.Network.Subnets[1], "2001:db8::/64")
	}
}

func TestRoundtrip_BGPCloudConfigurationAllPlatforms(t *testing.T) {
	for _, p := range networkingapi.AllPlatforms {
		t.Run(string(p), func(t *testing.T) {
			obj := buildForPlatform(t, "rt-"+dasherize(string(p)), p)
			if err := testClient.Create(ctx, obj); err != nil {
				t.Fatalf("create %s: %v", p, err)
			}
			t.Cleanup(func() { _ = testClient.Delete(ctx, obj) })

			fetched := &networkingapi.BGPCloudConfiguration{}
			if err := testClient.Get(ctx, types.NamespacedName{Name: obj.Name}, fetched); err != nil {
				t.Fatalf("get: %v", err)
			}
			if fetched.Spec.Platform != p {
				t.Errorf("platform = %v, want %v", fetched.Spec.Platform, p)
			}
		})
	}
}

func buildForPlatform(t *testing.T, name string, p networkingapi.PlatformType) *networkingapi.BGPCloudConfiguration {
	t.Helper()
	obj := &networkingapi.BGPCloudConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: networkingapi.BGPCloudConfigurationSpec{
			Platform:           p,
			BGP:                networkingapi.BGPConfig{LocalASN: 65001},
			RouterNodeSelector: map[string]string{"role": "router"},
		},
	}
	switch p {
	case networkingapi.PlatformAWS:
		obj.Spec.AWS = &networkingapi.AWSConfig{
			Region:         "us-east-1",
			RouteServerIDs: []string{"rs-1"},
		}
	case networkingapi.PlatformAzure:
		obj.Spec.Azure = &networkingapi.AzureConfig{
			SubscriptionID:  "sub-1",
			ResourceGroup:   "rg-1",
			RouteServerName: "rs-1",
		}
	case networkingapi.PlatformGCP:
		nested := true
		obj.Spec.GCP = &networkingapi.GCPConfig{
			Project:                    "p",
			Region:                     "r",
			CloudRouterName:            "cr",
			NCC:                        networkingapi.NCCConfig{HubName: "h", SpokePrefix: "s"},
			EnableNestedVirtualization: &nested,
		}
	case networkingapi.PlatformManual:
		obj.Spec.BGP.PeerGroups = []networkingapi.PeerGroup{{
			NodeSelector: map[string]string{"zone": "a"},
			Neighbors:    []networkingapi.BGPNeighbor{{Address: "10.0.0.1", RemoteASN: 64512}},
		}}
	default:
		t.Fatalf("unhandled platform: %s", p)
	}
	return obj
}

func dasherize(s string) string {
	out := make([]byte, 0, len(s))
	for i := range s {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				out = append(out, '-')
			}
			out = append(out, c+32)
		} else {
			out = append(out, c)
		}
	}
	return string(out)
}
