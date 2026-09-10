package gcp

import (
	"context"
	"errors"
	"testing"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

var errGCP = errors.New("gcp API failure")

type errTopology struct{ recordingCompute }

func (*errTopology) GetRouterTopology(context.Context, string) (*CloudRouterTopology, error) {
	return nil, errGCP
}

type errForward struct{ recordingCompute }

func (*errForward) EnsureCanIPForward(context.Context, RouterNode) (bool, error) {
	return false, errGCP
}

type errPeers struct{ recordingCompute }

func (*errPeers) ReconcilePeers(context.Context, string, string, []RouterNode, *CloudRouterTopology, int) (bool, error) {
	return false, errGCP
}

type errSpokes struct{ recordingNCC }

func (*errSpokes) ReconcileSpoke(context.Context, string, string, []RouterNode, bool) (bool, error) {
	return false, errGCP
}

func testMetricsNodes() []platform.RouterNode {
	return []platform.RouterNode{{
		Name:       "worker-a",
		ProviderID: "gce://openshift-qe/us-east1-b/worker-a",
		Zone:       "us-east1-b",
		PrivateIP:  "10.0.128.2",
	}}
}

// TestGCPMetrics_APIErrorsIncrement covers every operation label GCP records
// against cloud_api_errors_total, including the NCC spoke calls that have no
// counterpart on the other clouds.
func TestGCPMetrics_APIErrorsIncrement(t *testing.T) {
	cases := []struct {
		op   string
		fail func(*testing.T)
	}{
		{
			op: platform.OpDiscover,
			fail: func(t *testing.T) {
				p := testPlatform(&errTopology{}, &recordingNCC{})
				if _, err := p.DiscoverEndpoints(context.Background()); err == nil {
					t.Fatal("expected error")
				}
			},
		},
		{
			op: platform.OpNodeForwarding,
			fail: func(t *testing.T) {
				p := testPlatform(&errForward{}, &recordingNCC{})
				if err := p.ReconcileNodes(context.Background(), testMetricsNodes()); err == nil {
					t.Fatal("expected error")
				}
			},
		},
		{
			op: platform.OpNCC,
			fail: func(t *testing.T) {
				p := testPlatform(&recordingCompute{topology: testTopology()}, &errSpokes{})
				if err := p.ReconcileNodes(context.Background(), testMetricsNodes()); err == nil {
					t.Fatal("expected error")
				}
			},
		},
		{
			op: platform.OpPeer,
			fail: func(t *testing.T) {
				p := testPlatform(&errPeers{recordingCompute{topology: testTopology()}}, &recordingNCC{})
				if err := p.ReconcileNodes(context.Background(), testMetricsNodes()); err == nil {
					t.Fatal("expected error")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			before := platform.APIErrorCount(platform.PlatformGCP, tc.op)
			tc.fail(t)
			if got := platform.APIErrorCount(platform.PlatformGCP, tc.op); got != before+1 {
				t.Fatalf("%s errors: got %v, want %v", tc.op, got, before+1)
			}
		})
	}
}

// TestGCPMetrics_PeersManagedGauge counts a peer per Cloud Router interface,
// not per node: every router node peers with every interface, so a gauge that
// counted nodes would report half the sessions that exist.
func TestGCPMetrics_PeersManagedGauge(t *testing.T) {
	p := testPlatform(&recordingCompute{topology: testTopology()}, &recordingNCC{})

	if err := p.ReconcileNodes(context.Background(), testMetricsNodes()); err != nil {
		t.Fatalf("ReconcileNodes: %v", err)
	}
	// One node against the two interfaces of testTopology.
	if got := platform.PeersManagedValue(platform.PlatformGCP); got != 2 {
		t.Fatalf("cloud_peers_managed: got %v, want 2", got)
	}

	if err := p.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if got := platform.PeersManagedValue(platform.PlatformGCP); got != 0 {
		t.Fatalf("cloud_peers_managed after cleanup: got %v, want 0", got)
	}
}
