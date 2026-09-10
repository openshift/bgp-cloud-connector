package azure

import (
	"context"
	"fmt"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// DiscoverEndpoints returns a single peer group covering every router node.
//
// An Azure Route Server is regional and presents the same redundant pair of
// addresses to every router node, so there is nothing for a zone axis to
// partition and the group carries no node selector. AWS, whose endpoints are
// per subnet and therefore per zone, is the one that emits a group per zone.
//
// Every neighbour asks for eBGP multihop, because the Route Server sits in its
// own subnet rather than on the node's link.
func (p *Platform) DiscoverEndpoints(ctx context.Context) (*platform.DiscoveryResult, error) {
	topology, err := p.topo.GetTopology(ctx)
	if err != nil {
		platform.RecordCloudAPIError(platform.PlatformAzure, platform.OpDiscover)
		return nil, fmt.Errorf("reading Route Server %q: %w", p.cfg.RouteServerName, err)
	}
	// Neither refusal below is counted: Azure answered, and what it said is
	// that this Route Server cannot be peered with. That is a cloud
	// configuration problem, not a cloud API failure.
	if len(topology.Addresses) == 0 {
		return nil, fmt.Errorf("no addresses to peer with on Route Server %q", p.cfg.RouteServerName)
	}
	if topology.ASN == 0 {
		return nil, fmt.Errorf("no ASN on Route Server %q, cannot peer", p.cfg.RouteServerName)
	}

	group := platform.PeerGroup{Key: p.cfg.RouteServerName}
	for _, addr := range topology.Addresses {
		group.Neighbors = append(group.Neighbors, platform.DiscoveredNeighbor{
			Address:      addr,
			ASN:          topology.ASN,
			EBGPMultiHop: true,
		})
	}

	return &platform.DiscoveryResult{PeerGroups: []platform.PeerGroup{group}}, nil
}
