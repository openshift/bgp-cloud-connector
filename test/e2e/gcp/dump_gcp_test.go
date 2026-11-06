package gcp_e2e

import (
	"context"
	"testing"

	networkingapi "github.com/openshift/bgp-cloud-connector/api/v1beta1"
)

// BeforeSuite builds the Kubernetes clients before the Google ones, so
// a failure building the Google clients reaches the diagnostics with
// the profile loaded and computeSvc and nccSvc still nil. dumpGCP must
// say so rather than panic and take the sections after it down too.
func TestDumpGCPWithoutGoogleClients(t *testing.T) {
	savedConfig, savedCompute, savedNCC := bgpConfig, computeSvc, nccSvc
	t.Cleanup(func() { bgpConfig, computeSvc, nccSvc = savedConfig, savedCompute, savedNCC })

	bgpConfig = &networkingapi.BGPCloudConfiguration{}
	bgpConfig.Spec.GCP = &networkingapi.GCPConfig{
		Project: "p", Region: "r", CloudRouterName: "cr",
		NCC: networkingapi.NCCConfig{HubName: "hub"},
	}
	computeSvc, nccSvc = nil, nil

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("dumpGCP panicked without Google clients: %v", r)
		}
	}()
	dumpGCP(context.Background())
}
