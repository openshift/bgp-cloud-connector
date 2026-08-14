package aws

import (
	"context"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

func (p *Platform) reconcileRouteServerPeers(ctx context.Context, nodes []platform.RouterNode) error {
	logger := log.FromContext(ctx)

	nodesByAZ := make(map[string]map[string]bool)
	for _, n := range nodes {
		if nodesByAZ[n.AZ] == nil {
			nodesByAZ[n.AZ] = make(map[string]bool)
		}
		nodesByAZ[n.AZ][n.PrivateIP] = true
	}

	// DescribeRouteServerPeers accepts no endpoint filter, so one
	// unfiltered call already returns every peer we could care about.
	// Asking again per endpoint made 2N identical full-region calls per
	// reconcile against rate buckets shared by the whole account.
	peersByEndpoint, err := p.listPeersByEndpoint(ctx)
	if err != nil {
		return fmt.Errorf("listing route server peers: %w", err)
	}

	tagKey := managedByTagKey()
	tagVal := p.peerTagValue()

	// Sorted throughout: endpointsByAZ is a map, so unsorted iteration
	// made the order of creates and deletes differ on every reconcile,
	// which left logs and failures impossible to reproduce.
	for _, az := range sortedKeys(p.endpointsByAZ) {
		desiredIPs := nodesByAZ[az]

		for _, endpointID := range p.endpointsByAZ[az] {
			managedIPs := make(map[string]string)
			unmanagedIPs := make(map[string]string)

			for _, peer := range peersByEndpoint[endpointID] {
				if peer.PeerAddress == nil || peer.RouteServerPeerId == nil {
					continue
				}
				if hasTag(peer.Tags, tagKey, tagVal) {
					managedIPs[*peer.PeerAddress] = *peer.RouteServerPeerId
				} else {
					unmanagedIPs[*peer.PeerAddress] = *peer.RouteServerPeerId
				}
			}

			for _, ip := range sortedStringKeys(managedIPs) {
				if !desiredIPs[ip] {
					peerID := managedIPs[ip]
					logger.Info("deleting stale route server peer", "endpoint", endpointID, "peerIP", ip, "peerID", peerID, "az", az)
					if err := p.deletePeer(ctx, peerID); err != nil {
						return fmt.Errorf("deleting peer %s: %w", peerID, err)
					}
				}
			}

			for _, ip := range sortedBoolKeys(desiredIPs) {
				if _, exists := managedIPs[ip]; exists {
					continue
				}
				if peerID, exists := unmanagedIPs[ip]; exists {
					logger.Info("adopting pre-existing route server peer", "endpoint", endpointID, "peerIP", ip, "peerID", peerID, "az", az)
					if err := p.tagPeer(ctx, peerID); err != nil {
						return fmt.Errorf("tagging peer %s: %w", peerID, err)
					}
				} else {
					logger.Info("creating route server peer", "endpoint", endpointID, "peerIP", ip, "az", az)
					if err := p.createPeer(ctx, endpointID, ip); err != nil {
						return fmt.Errorf("creating peer for %s on %s: %w", ip, endpointID, err)
					}
				}
			}
		}
	}

	return nil
}

// listPeersByEndpoint fetches every live peer once and buckets it by
// endpoint, so a reconcile costs one API call rather than one per endpoint.
func (p *Platform) listPeersByEndpoint(ctx context.Context) (map[string][]ec2types.RouteServerPeer, error) {
	output, err := p.ec2Client.DescribeRouteServerPeers(ctx, &ec2.DescribeRouteServerPeersInput{})
	if err != nil {
		return nil, err
	}
	byEndpoint := make(map[string][]ec2types.RouteServerPeer)
	for _, peer := range output.RouteServerPeers {
		if !peerIsAlive(peer.State) {
			continue
		}
		id := aws.ToString(peer.RouteServerEndpointId)
		byEndpoint[id] = append(byEndpoint[id], peer)
	}
	return byEndpoint, nil
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedStringKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (p *Platform) listManagedPeers(ctx context.Context, endpointID string) ([]ec2types.RouteServerPeer, error) {
	all, err := p.listAllPeers(ctx, endpointID)
	if err != nil {
		return nil, err
	}
	tagKey := managedByTagKey()
	tagVal := p.peerTagValue()
	var filtered []ec2types.RouteServerPeer
	for _, peer := range all {
		if hasTag(peer.Tags, tagKey, tagVal) {
			filtered = append(filtered, peer)
		}
	}
	return filtered, nil
}

func (p *Platform) listAllPeers(ctx context.Context, endpointID string) ([]ec2types.RouteServerPeer, error) {
	output, err := p.ec2Client.DescribeRouteServerPeers(ctx, &ec2.DescribeRouteServerPeersInput{})
	if err != nil {
		return nil, err
	}
	var filtered []ec2types.RouteServerPeer
	for _, peer := range output.RouteServerPeers {
		if aws.ToString(peer.RouteServerEndpointId) != endpointID {
			continue
		}
		if !peerIsAlive(peer.State) {
			continue
		}
		filtered = append(filtered, peer)
	}
	return filtered, nil
}

// peerIsAlive reports whether a peer exists or is on its way to existing.
//
// DescribeRouteServerPeers keeps returning peers long after they are gone,
// in states such as "deleted". Treating those as present makes us believe
// BGP is configured when no session can form, and makes us try to delete
// something AWS has already deleted, which fails with IncorrectState.
//
// This is an allowlist so that a state AWS introduces later is treated as
// gone, and we recreate, rather than being silently counted as working.
// "pending" is alive because peers sit there for minutes after creation
// and must not be duplicated on the next resync.
func peerIsAlive(state ec2types.RouteServerPeerState) bool {
	switch state {
	case ec2types.RouteServerPeerStateAvailable, ec2types.RouteServerPeerStatePending:
		return true
	default:
		return false
	}
}

func hasTag(tags []ec2types.Tag, key, value string) bool {
	for _, t := range tags {
		if aws.ToString(t.Key) == key && aws.ToString(t.Value) == value {
			return true
		}
	}
	return false
}

func (p *Platform) tagPeer(ctx context.Context, peerID string) error {
	input := &ec2.CreateTagsInput{
		Resources: []string{peerID},
		Tags:      p.peerTags(),
	}
	_, err := p.ec2Client.CreateTags(ctx, input)
	return err
}

func (p *Platform) createPeer(ctx context.Context, endpointID, peerAddress string) error {
	bgpOpts := &ec2types.RouteServerBgpOptionsRequest{
		PeerAsn: aws.Int64(p.localASN),
	}
	if p.livenessDetection == "bfd" {
		bgpOpts.PeerLivenessDetection = ec2types.RouteServerPeerLivenessMode("bfd")
	}
	input := &ec2.CreateRouteServerPeerInput{
		RouteServerEndpointId: aws.String(endpointID),
		PeerAddress:           aws.String(peerAddress),
		BgpOptions:            bgpOpts,
		TagSpecifications: []ec2types.TagSpecification{
			{
				ResourceType: ec2types.ResourceTypeRouteServerPeer,
				Tags:         p.peerTags(),
			},
		},
	}
	_, err := p.ec2Client.CreateRouteServerPeer(ctx, input)
	return err
}

func (p *Platform) deletePeer(ctx context.Context, peerID string) error {
	input := &ec2.DeleteRouteServerPeerInput{
		RouteServerPeerId: aws.String(peerID),
	}
	_, err := p.ec2Client.DeleteRouteServerPeer(ctx, input)
	return err
}

func (p *Platform) deleteAllManagedPeers(ctx context.Context) error {
	for _, endpointIDs := range p.endpointsByAZ {
		for _, endpointID := range endpointIDs {
			peers, err := p.listManagedPeers(ctx, endpointID)
			if err != nil {
				return fmt.Errorf("listing peers for endpoint %s: %w", endpointID, err)
			}
			for _, peer := range peers {
				if peer.RouteServerPeerId != nil {
					if err := p.deletePeer(ctx, *peer.RouteServerPeerId); err != nil {
						return fmt.Errorf("deleting peer %s: %w", *peer.RouteServerPeerId, err)
					}
				}
			}
		}
	}
	return nil
}
