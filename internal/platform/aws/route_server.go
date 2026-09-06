package aws

import (
	"context"
	"fmt"

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
		if nodesByAZ[n.Zone] == nil {
			nodesByAZ[n.Zone] = make(map[string]bool)
		}
		nodesByAZ[n.Zone][n.PrivateIP] = true
	}

	for az, endpointIDs := range p.endpointsByAZ {
		desiredIPs := nodesByAZ[az]

		for _, endpointID := range endpointIDs {
			managedPeers, err := p.listManagedPeers(ctx, endpointID)
			if err != nil {
				return fmt.Errorf("listing managed peers for endpoint %s: %w", endpointID, err)
			}

			managedIPs := make(map[string]string, len(managedPeers))
			deleting := make(map[string]bool, len(managedPeers))
			for _, peer := range managedPeers {
				if peer.PeerAddress == nil || peer.RouteServerPeerId == nil {
					continue
				}
				// A deleted peer is not one that exists, however long EC2 goes
				// on listing it.
				if isDeleted(peer) {
					continue
				}
				managedIPs[*peer.PeerAddress] = *peer.RouteServerPeerId
				if isDeleting(peer) {
					deleting[*peer.RouteServerPeerId] = true
				}
			}

			for ip, peerID := range managedIPs {
				if desiredIPs[ip] || deleting[peerID] {
					continue
				}
				logger.Info("deleting stale route server peer", "endpoint", endpointID, "peerIP", ip, "peerID", peerID, "az", az)
				if err := p.deletePeer(ctx, peerID); err != nil {
					return fmt.Errorf("deleting peer %s: %w", peerID, err)
				}
			}

			allPeers, err := p.listAllPeers(ctx, endpointID)
			if err != nil {
				return fmt.Errorf("listing all peers for endpoint %s: %w", endpointID, err)
			}
			unmanagedIPs := make(map[string]string, len(allPeers))
			for _, peer := range allPeers {
				if peer.PeerAddress == nil || peer.RouteServerPeerId == nil {
					continue
				}
				// Adopting a peer that is on its way out, or gone, would tag
				// something that is about to stop existing and leave the node
				// with no peer at all.
				if isDeleting(peer) || isDeleted(peer) {
					continue
				}
				ip := *peer.PeerAddress
				if _, managed := managedIPs[ip]; !managed {
					unmanagedIPs[ip] = *peer.RouteServerPeerId
				}
			}

			for ip := range desiredIPs {
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

	managed, err := p.countManagedPeers(ctx, nodesByAZ)
	if err != nil {
		return err
	}
	awsPeersManaged.Set(float64(managed))
	return nil
}

func (p *Platform) countManagedPeers(ctx context.Context, nodesByAZ map[string]map[string]bool) (int, error) {
	managed := 0
	for az, endpointIDs := range p.endpointsByAZ {
		desiredIPs := nodesByAZ[az]
		for _, endpointID := range endpointIDs {
			peers, err := p.listManagedPeers(ctx, endpointID)
			if err != nil {
				return 0, fmt.Errorf("listing managed peers for endpoint %s: %w", endpointID, err)
			}
			for _, peer := range peers {
				if peer.PeerAddress == nil || isDeleted(peer) {
					continue
				}
				if desiredIPs[*peer.PeerAddress] {
					managed++
				}
			}
		}
	}
	return managed, nil
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
	var filtered []ec2types.RouteServerPeer
	var nextToken *string
	for {
		output, err := p.ec2Client.DescribeRouteServerPeers(ctx, &ec2.DescribeRouteServerPeersInput{
			NextToken: nextToken,
		})
		if err != nil {
			recordAWSAPIError(opPeer)
			return nil, err
		}
		for _, peer := range output.RouteServerPeers {
			if aws.ToString(peer.RouteServerEndpointId) == endpointID {
				filtered = append(filtered, peer)
			}
		}
		if aws.ToString(output.NextToken) == "" {
			break
		}
		nextToken = output.NextToken
	}
	return filtered, nil
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
	if err != nil {
		recordAWSAPIError(opPeer)
	}
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
	if err != nil {
		recordAWSAPIError(opPeer)
	}
	return err
}

func (p *Platform) deletePeer(ctx context.Context, peerID string) error {
	input := &ec2.DeleteRouteServerPeerInput{
		RouteServerPeerId: aws.String(peerID),
	}
	_, err := p.ec2Client.DeleteRouteServerPeer(ctx, input)
	if err != nil {
		recordAWSAPIError(opPeer)
	}
	return err
}

// EC2 keeps returning a route server peer after it has been deleted, and
// refuses a delete on one that is deleting or deleted with IncorrectState. The
// two states are not the same thing, and treating them alike gets one of the
// two paths wrong.
//
// deleting: still holds its address, so it counts as one already there and no
// replacement is built beside it, but its delete must not be reissued.
//
// deleted: holds nothing. It must not count as one already there, or a peer
// removed out from under the operator is never replaced and drift is never
// repaired.
func isDeleting(peer ec2types.RouteServerPeer) bool {
	return peer.State == ec2types.RouteServerPeerStateDeleting
}

func isDeleted(peer ec2types.RouteServerPeer) bool {
	return peer.State == ec2types.RouteServerPeerStateDeleted
}

func (p *Platform) deleteAllManagedPeers(ctx context.Context) error {
	for _, endpointIDs := range p.endpointsByAZ {
		for _, endpointID := range endpointIDs {
			peers, err := p.listManagedPeers(ctx, endpointID)
			if err != nil {
				return fmt.Errorf("listing peers for endpoint %s: %w", endpointID, err)
			}
			for _, peer := range peers {
				if peer.RouteServerPeerId == nil || isDeleting(peer) || isDeleted(peer) {
					continue
				}
				if err := p.deletePeer(ctx, *peer.RouteServerPeerId); err != nil {
					return fmt.Errorf("deleting peer %s: %w", *peer.RouteServerPeerId, err)
				}
			}
		}
	}
	awsPeersManaged.Set(0)
	return nil
}
