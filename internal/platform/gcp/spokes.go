package gcp

import (
	"context"
	"errors"
	"sort"
	"strconv"
)

// NCCMaxInstancesPerSpoke is the GCP limit for router appliance instances
// per NCC spoke.
const NCCMaxInstancesPerSpoke = 8

// ChunkRouterNodes splits nodes into chunks of at most maxPer (GCP NCC limit).
func ChunkRouterNodes(nodes []RouterNode, maxPer int) [][]RouterNode {
	if maxPer <= 0 {
		maxPer = 1
	}
	sorted := append([]RouterNode(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	var out [][]RouterNode
	for i := 0; i < len(sorted); i += maxPer {
		end := i + maxPer
		if end > len(sorted) {
			end = len(sorted)
		}
		out = append(out, sorted[i:end])
	}
	return out
}

// ReconcileNCCSpokes creates/updates numbered spokes and deletes stale ones.
func ReconcileNCCSpokes(ctx context.Context, ncc NCCClient, hubName, prefix string, siteToSite bool, routerNodes []RouterNode) (int, error) {
	chunks := ChunkRouterNodes(routerNodes, NCCMaxInstancesPerSpoke)
	desiredIDs := make([]string, len(chunks))
	desiredSet := make(map[string]struct{})
	for i := range chunks {
		id := prefix + "-" + strconv.Itoa(i)
		desiredIDs[i] = id
		desiredSet[id] = struct{}{}
	}
	changes := 0
	var mismatches []error
	for i, id := range desiredIDs {
		changed, err := ncc.ReconcileSpoke(ctx, id, hubName, chunks[i], siteToSite)
		if changed {
			changes++
		}
		if errors.Is(err, errSiteToSiteFixed) {
			mismatches = append(mismatches, err)
			continue
		}
		if err != nil {
			return changes, err
		}
	}
	existing, err := ncc.ListSpokesByPrefix(ctx, hubName, prefix)
	if err != nil {
		return changes, err
	}
	for _, spokeID := range existing {
		if _, ok := desiredSet[spokeID]; ok {
			continue
		}
		deleted, err := ncc.DeleteSpoke(ctx, spokeID)
		if err != nil {
			return changes, err
		}
		if deleted {
			changes++
		}
	}
	return changes, errors.Join(mismatches...)
}
