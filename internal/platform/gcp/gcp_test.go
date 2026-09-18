package gcp

import (
	"context"
	"strings"
	"testing"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// recordingCompute records what ReconcileNodes asks of the Cloud Router.
type recordingCompute struct {
	topology       *CloudRouterTopology
	reconcilePeers int
	clearPeers     int
}

func (c *recordingCompute) EnsureCanIPForward(context.Context, RouterNode) (bool, error) {
	return false, nil
}

func (c *recordingCompute) EnsureNestedVirtualization(context.Context, RouterNode) (bool, error) {
	return false, nil
}

func (c *recordingCompute) GetRouterTopology(context.Context, string) (*CloudRouterTopology, error) {
	return c.topology, nil
}

func (c *recordingCompute) ReconcilePeers(context.Context, string, string, []RouterNode, *CloudRouterTopology, int) (bool, error) {
	c.reconcilePeers++
	return false, nil
}

func (c *recordingCompute) ClearPeers(context.Context, string, string) (bool, error) {
	c.clearPeers++
	return false, nil
}

// recordingNCC reports the spokes that already exist and records every call
// that would change them.
type recordingNCC struct {
	existing []string
	deleted  []string
	updated  []string
}

func (n *recordingNCC) ReconcileSpoke(_ context.Context, spokeName, _ string, _ []RouterNode, _ bool) (bool, error) {
	n.updated = append(n.updated, spokeName)
	return false, nil
}

func (n *recordingNCC) DeleteSpoke(_ context.Context, spokeName string) (bool, error) {
	n.deleted = append(n.deleted, spokeName)
	return true, nil
}

func (n *recordingNCC) ListSpokesByPrefix(context.Context, string, string) ([]string, error) {
	return n.existing, nil
}

func testPlatform(compute ComputeClient, ncc NCCClient) *Platform {
	return &Platform{
		cfg: Config{
			CloudRouterName: "cluster-cudn-cr",
			NCCHubName:      "cluster-ncc-hub",
			NCCSpokePrefix:  "cluster-bgp-spoke",
			LocalASN:        65001,
			ClusterID:       "cluster",
		},
		compute: compute,
		ncc:     ncc,
	}
}

func testTopology() *CloudRouterTopology {
	return &CloudRouterTopology{
		CloudRouterASN: 65000,
		InterfaceNames: []string{"if-a", "if-b"},
		InterfaceIPs:   []string{"169.254.1.1", "169.254.1.5"},
	}
}

// TestReconcileNodes_EmptyNodeListLeavesCloudAlone covers a router node
// selector that momentarily matches nothing, which happens whenever the label
// is being moved during a rollout.
//
// Reconciling an empty set the same way as a shrunk one deletes every spoke
// and writes a peer list with none of ours in it, so a transient gap in the
// node list tears down BGP for the whole cluster. Releasing the estate is what
// Cleanup is for, and it runs on deletion, where the intent is unambiguous.
func TestReconcileNodes_EmptyNodeListLeavesCloudAlone(t *testing.T) {
	compute := &recordingCompute{topology: testTopology()}
	ncc := &recordingNCC{existing: []string{"cluster-bgp-spoke-0", "cluster-bgp-spoke-1"}}

	if err := testPlatform(compute, ncc).ReconcileNodes(context.Background(), nil); err != nil {
		t.Fatalf("ReconcileNodes on an empty node list: %v", err)
	}

	if len(ncc.deleted) != 0 {
		t.Errorf("deleted spokes %v, want none: an empty node list must not release the estate", ncc.deleted)
	}
	if compute.reconcilePeers != 0 {
		t.Errorf("ReconcilePeers called %d times, want 0: writing an empty peer set removes every owned peer", compute.reconcilePeers)
	}
}

// TestReconcileNodes_ShrunkNodeListStillReconciles guards the early return
// from swallowing the case it must not: losing one node of several still has
// to reach the Cloud Router, or a removed node keeps its peer forever.
func TestReconcileNodes_ShrunkNodeListStillReconciles(t *testing.T) {
	compute := &recordingCompute{topology: testTopology()}
	ncc := &recordingNCC{existing: []string{"cluster-bgp-spoke-0", "cluster-bgp-spoke-1"}}

	nodes := []platform.RouterNode{{
		Name:       "worker-a",
		ProviderID: "gce://openshift-qe/us-east1-b/worker-a",
		Zone:       "us-east1-b",
		PrivateIP:  "10.0.128.2",
	}}

	if err := testPlatform(compute, ncc).ReconcileNodes(context.Background(), nodes); err != nil {
		t.Fatalf("ReconcileNodes: %v", err)
	}

	if compute.reconcilePeers != 1 {
		t.Errorf("ReconcilePeers called %d times, want 1", compute.reconcilePeers)
	}
	if len(ncc.updated) != 1 || ncc.updated[0] != "cluster-bgp-spoke-0" {
		t.Errorf("reconciled spokes %v, want [cluster-bgp-spoke-0]: one node needs one spoke", ncc.updated)
	}
	if len(ncc.deleted) != 1 || ncc.deleted[0] != "cluster-bgp-spoke-1" {
		t.Errorf("deleted spokes %v, want [cluster-bgp-spoke-1]: the second spoke is now surplus", ncc.deleted)
	}
}

// TestDiscoverEndpoints_BuildsOneGroupPerRouter pins that every Cloud Router
// interface becomes a neighbor in a single peer group. Unlike AWS there is no
// per-zone split, so one group with no node selector covers every router node.
func TestDiscoverEndpoints_BuildsOneGroupPerRouter(t *testing.T) {
	top := testTopology()
	p := testPlatform(&recordingCompute{topology: top}, &recordingNCC{})

	result, err := p.DiscoverEndpoints(context.Background())
	if err != nil {
		t.Fatalf("DiscoverEndpoints: %v", err)
	}
	if len(result.PeerGroups) != 1 {
		t.Fatalf("got %d peer groups, want 1", len(result.PeerGroups))
	}
	group := result.PeerGroups[0]
	if len(group.Neighbors) != len(top.InterfaceIPs) {
		t.Fatalf("got %d neighbors, want %d", len(group.Neighbors), len(top.InterfaceIPs))
	}
	for i, n := range group.Neighbors {
		if n.Address != top.InterfaceIPs[i] {
			t.Errorf("neighbor %d address = %q, want %q", i, n.Address, top.InterfaceIPs[i])
		}
		if n.ASN != top.CloudRouterASN {
			t.Errorf("neighbor %d ASN = %d, want %d", i, n.ASN, top.CloudRouterASN)
		}
	}
	// The structured neighbor API cannot express disable-connected-check, without
	// which FRR rejects the Cloud Router interface as unreachable and no session
	// comes up, so it has to ride along as raw config.
	if !strings.Contains(group.RawFRRConfig, "disable-connected-check") {
		t.Errorf("raw FRR config is missing disable-connected-check:\n%s", group.RawFRRConfig)
	}
}

// TestCleanup_ClearsPeersAndDeletesEverySpoke pins that Cleanup, which runs on
// deletion where the intent to release the estate is unambiguous, removes the
// Cloud Router peers and every spoke the prefix owns.
func TestCleanup_ClearsPeersAndDeletesEverySpoke(t *testing.T) {
	compute := &recordingCompute{topology: testTopology()}
	ncc := &recordingNCC{existing: []string{"cluster-bgp-spoke-0", "cluster-bgp-spoke-1"}}

	if err := testPlatform(compute, ncc).Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if compute.clearPeers != 1 {
		t.Errorf("ClearPeers called %d times, want 1", compute.clearPeers)
	}
	if len(ncc.deleted) != 2 {
		t.Errorf("deleted spokes %v, want both existing spokes released", ncc.deleted)
	}
}

func TestRawFRRConfig_EmitsASNAndDisableConnectedCheck(t *testing.T) {
	cfg := rawFRRConfig(65001, []string{"169.254.1.1", "169.254.1.5"})
	if !strings.Contains(cfg, "router bgp 65001") {
		t.Errorf("config is missing the local ASN line:\n%s", cfg)
	}
	for _, ip := range []string{"169.254.1.1", "169.254.1.5"} {
		if !strings.Contains(cfg, "neighbor "+ip+" disable-connected-check") {
			t.Errorf("config is missing disable-connected-check for %s:\n%s", ip, cfg)
		}
	}
}
