package gcp_e2e

import (
	"testing"

	"google.golang.org/api/compute/v1"

	gcpplatform "github.com/openshift/bgp-cloud-connector/internal/platform/gcp"
)

// The suite has to claim exactly the peers the operator claims: a peer
// it misses makes the spoke and peer checks fail, and one it wrongly
// claims, or none at all, lets the "peers have gone" check pass without
// checking anything.
func TestPeersOf(t *testing.T) {
	ours := gcpplatform.PeerName("cluster", "10.0.0.1", 0)
	other := gcpplatform.PeerName("cluster-bgpx", "10.0.0.2", 1)
	peers := []*compute.RouterBgpPeer{{Name: ours}, {Name: other}, {Name: "hand-made"}}

	got, err := peersOf(peers, "cluster")
	if err != nil {
		t.Fatalf("peersOf: %v", err)
	}
	if len(got) != 1 || got[0].Name != ours {
		var names []string
		for _, p := range got {
			names = append(names, p.Name)
		}
		t.Errorf("got peers %v, want only [%s]", names, ours)
	}
}

// Every prefix PeerName can produce, including an abbreviated one, must
// survive the trim, or the suite claims nothing.
func TestPeersOfLongClusterName(t *testing.T) {
	long := "a-cluster-name-well-past-the-thirty-four-character-prefix-limit"
	ours := gcpplatform.PeerName(long, "10.0.0.1", 0)
	got, err := peersOf([]*compute.RouterBgpPeer{{Name: ours}}, long)
	if err != nil {
		t.Fatalf("peersOf: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d peers, want 1 (%s)", len(got), ours)
	}
}
