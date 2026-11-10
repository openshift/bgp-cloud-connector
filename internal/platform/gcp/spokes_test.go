package gcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
)

// TestChunkRouterNodes covers the sort and the split at the NCC instance
// limit.
func TestChunkRouterNodes(t *testing.T) {
	mk := func(names ...string) []RouterNode {
		var out []RouterNode
		for _, n := range names {
			out = append(out, RouterNode{Name: n})
		}
		return out
	}

	chunks := ChunkRouterNodes(mk("b", "a", "c"), NCCMaxInstancesPerSpoke)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	if len(chunks[0]) != 3 {
		t.Fatalf("got %d nodes in the chunk, want 3", len(chunks[0]))
	}
	if chunks[0][0].Name != "a" {
		t.Errorf("first node = %q, want %q: chunks are sorted by name", chunks[0][0].Name, "a")
	}

	chunks = ChunkRouterNodes(mk("1", "2", "3", "4", "5", "6", "7", "8", "9"), 8)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if len(chunks[0]) != 8 {
		t.Errorf("first chunk has %d nodes, want 8", len(chunks[0]))
	}
	if len(chunks[1]) != 1 {
		t.Errorf("second chunk has %d nodes, want 1", len(chunks[1]))
	}
	if chunks[1][0].Name != "9" {
		t.Errorf("overflow node = %q, want %q", chunks[1][0].Name, "9")
	}
}

// mismatchedNCC reports a site-to-site mismatch on one spoke, after
// patching its instances, and reconciles every other spoke.
type mismatchedNCC struct {
	recordingNCC
	mismatched string
}

func (n *mismatchedNCC) ReconcileSpoke(ctx context.Context, spokeName, hubName string, nodes []RouterNode, siteToSite bool) (bool, error) {
	if _, err := n.recordingNCC.ReconcileSpoke(ctx, spokeName, hubName, nodes, siteToSite); err != nil {
		return false, err
	}
	if spokeName == n.mismatched {
		return true, fmt.Errorf("NCC spoke %s: %w", spokeName, errSiteToSiteFixed)
	}
	return true, nil
}

// A site-to-site mismatch is reported by one spoke but says nothing
// about the others, so the spokes after it are still reconciled, a
// surplus spoke is still deleted, and the mismatch is returned at the
// end with every change counted.
func TestReconcileNCCSpokesCarriesOnPastASiteToSiteMismatch(t *testing.T) {
	ncc := &mismatchedNCC{
		recordingNCC: recordingNCC{existing: []string{"spoke-0", "spoke-1", "spoke-2"}},
		mismatched:   "spoke-0",
	}
	nodes := make([]RouterNode, NCCMaxInstancesPerSpoke+1)
	for i := range nodes {
		nodes[i] = RouterNode{Name: fmt.Sprintf("worker-%02d", i)}
	}

	changes, err := ReconcileNCCSpokes(context.Background(), ncc, "hub", "spoke", false, nodes)
	if !errors.Is(err, errSiteToSiteFixed) {
		t.Errorf("ReconcileNCCSpokes error = %v, want the site-to-site mismatch", err)
	}
	if want := []string{"spoke-0", "spoke-1"}; !slices.Equal(ncc.updated, want) {
		t.Errorf("reconciled spokes %v, want %v", ncc.updated, want)
	}
	if want := []string{"spoke-2"}; !slices.Equal(ncc.deleted, want) {
		t.Errorf("deleted spokes %v, want %v", ncc.deleted, want)
	}
	if changes != 3 {
		t.Errorf("changes = %d, want 3: two spokes patched and one deleted", changes)
	}
}
