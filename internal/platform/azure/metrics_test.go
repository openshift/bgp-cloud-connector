package azure

import (
	"context"
	"errors"
	"testing"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

var errAzure = errors.New("azure API failure")

type errRS struct{ fakeRS }

func (*errRS) ListPeers(_ context.Context) ([]ObservedPeer, error) { return nil, errAzure }

type errNICs struct{ fakeNICs }

func (*errNICs) ListNICs(_ context.Context, _ string) ([]NIC, error) { return nil, errAzure }

type errTopo struct{}

func (*errTopo) GetTopology(_ context.Context) (*RouteServerTopology, error) { return nil, errAzure }

// TestAzureMetrics_APIErrorsIncrement covers every operation label Azure
// records against cloud_api_errors_total: each counts an Azure failure once.
func TestAzureMetrics_APIErrorsIncrement(t *testing.T) {
	vm, err := ParseProviderID(testProviderID)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		op   string
		fail func(*testing.T)
	}{
		{
			op: platform.OpDiscover,
			fail: func(t *testing.T) {
				p := &Platform{cfg: Config{RouteServerName: "rs"}, topo: &errTopo{}}
				if _, err := p.DiscoverEndpoints(context.Background()); err == nil {
					t.Fatal("expected error")
				}
			},
		},
		{
			op: platform.OpPeer,
			fail: func(t *testing.T) {
				p := &Platform{cfg: Config{ClusterID: "cluster"}, rs: &errRS{}}
				if _, err := p.reconcileOurPeerings(context.Background(), nil); err == nil {
					t.Fatal("expected error")
				}
			},
		},
		{
			op: platform.OpNodeForwarding,
			fail: func(t *testing.T) {
				p := &Platform{nics: &errNICs{}}
				if err := p.ensureNodesCanForward(context.Background(), []VirtualMachine{vm}); err == nil {
					t.Fatal("expected error")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.op, func(t *testing.T) {
			before := platform.APIErrorCount(platform.PlatformAzure, tc.op)
			tc.fail(t)
			if got := platform.APIErrorCount(platform.PlatformAzure, tc.op); got != before+1 {
				t.Fatalf("%s errors: got %v, want %v", tc.op, got, before+1)
			}
		})
	}
}

// TestAzureMetrics_PeersManagedGauge pins the gauge to this cluster's peerings:
// a Route Server reached by another cluster must not inflate it, and cleanup
// must take it to zero rather than leave a stale count behind.
func TestAzureMetrics_PeersManagedGauge(t *testing.T) {
	vm, err := ParseProviderID(testProviderID)
	if err != nil {
		t.Fatal(err)
	}

	rs := &fakeRS{current: []ObservedPeer{
		{Peer: Peer{Name: peeringName("cluster", "10.0.128.4"), PeerIP: "10.0.128.4", PeerASN: 65001}},
		{Peer: Peer{Name: "other-bgp-10-9-9-9", PeerIP: "10.9.9.9", PeerASN: 65001}},
	}}
	p := &Platform{
		cfg:  Config{ClusterID: "cluster", LocalASN: 65001, RouteServerName: "rs"},
		rs:   rs,
		nics: &fakeNICs{nics: []NIC{{Name: "nic-a", ResourceGroup: vm.ResourceGroup, VMID: vm.ID}}},
	}
	nodes := []platform.RouterNode{{Name: vm.Name, ProviderID: testProviderID, PrivateIP: "10.0.128.4"}}

	if err := p.ReconcileNodes(context.Background(), nodes); err != nil {
		t.Fatalf("ReconcileNodes: %v", err)
	}
	if got := platform.PeersManagedValue(platform.PlatformAzure); got != 1 {
		t.Fatalf("cloud_peers_managed: got %v, want 1 (ours only)", got)
	}

	if err := p.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if got := platform.PeersManagedValue(platform.PlatformAzure); got != 0 {
		t.Fatalf("cloud_peers_managed after cleanup: got %v, want 0", got)
	}
}
