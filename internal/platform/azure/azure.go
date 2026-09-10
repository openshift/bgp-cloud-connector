package azure

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// maxPeeringNameLength is Azure's limit for a BGP connection name.
const maxPeeringNameLength = 80

// Config is everything the Azure platform needs that the controller cannot
// work out for itself. It is spec.azure plus the two values every platform is
// given.
type Config struct {
	SubscriptionID  string
	ResourceGroup   string
	RouteServerName string
	// LocalASN is the ASN FRR runs with on the router nodes, which is what the
	// Route Server peerings are told to expect.
	LocalASN int64
	// ClusterID names the peerings, so that a Route Server reached by more
	// than one cluster does not have them confused.
	ClusterID string
	// NICClientID is the managed identity to use for network interface calls.
	// Empty means the same identity as everything else.
	NICClientID string
	// Credential authenticates every Azure call this platform makes,
	// except the network interface calls when NICClientID names a
	// different identity for those.
	//
	// Passed in rather than built here, because what it should be is a
	// property of the cluster the operator is running on -- a secret the
	// cloud credential operator wrote, or whatever a manager run from a
	// desk already has -- and only the caller can see that.
	Credential azcore.TokenCredential
}

// Platform reconciles Azure Route Server peerings and router node interfaces.
type Platform struct {
	cfg  Config
	rs   Backend
	topo TopologyReader
	nics NICClient
}

// New builds a Platform against the live Azure APIs.
func New(cfg Config) (*Platform, error) {
	rs, err := NewRouteServerBackend(cfg.SubscriptionID, cfg.ResourceGroup, cfg.RouteServerName, cfg.Credential)
	if err != nil {
		return nil, &platform.CredentialError{Msg: fmt.Sprintf("Azure Route Server client: %v", err)}
	}
	topo, err := NewTopologyReader(cfg.SubscriptionID, cfg.ResourceGroup, cfg.RouteServerName, cfg.Credential)
	if err != nil {
		return nil, &platform.CredentialError{Msg: fmt.Sprintf("Azure virtual hub client: %v", err)}
	}
	nics, err := NewNICClient(cfg.SubscriptionID, cfg.NICClientID, cfg.Credential)
	if err != nil {
		return nil, &platform.CredentialError{Msg: fmt.Sprintf("Azure network interface client: %v", err)}
	}
	return &Platform{cfg: cfg, rs: rs, topo: topo, nics: nics}, nil
}

// peeringPrefix is the ownership signal. An Azure BGP connection carries no
// tags, so as on GCP the name is the only marker available, and it is why
// nothing here touches a peering that does not carry it.
func peeringPrefix(clusterID string) string {
	return clusterID + "-bgp"
}

// peeringName keys a peering on the node's address, which is unique to it and
// unchanged when other nodes come and go. Rewriting a peering drops its
// session, so a name that moves with the node set would drop sessions that
// have no reason to change.
func peeringName(clusterID, address string) string {
	suffix := "-" + strings.ReplaceAll(address, ".", "-")
	prefix := peeringPrefix(clusterID)
	if len(prefix)+len(suffix) > maxPeeringNameLength {
		prefix = prefix[:maxPeeringNameLength-len(suffix)]
	}
	return prefix + suffix
}

// isOurPeering reports whether a peering was created for this cluster. The
// trailing separator matters: without it a cluster named "cluster" would claim
// the peerings of one named "cluster-two".
func isOurPeering(name, clusterID string) bool {
	return strings.HasPrefix(name, peeringPrefix(clusterID)+"-")
}

// ReconcileNodes brings router node interfaces and Route Server peerings into
// line with the given nodes.
func (p *Platform) ReconcileNodes(ctx context.Context, nodes []platform.RouterNode) error {
	logger := log.FromContext(ctx)

	// An empty node list is a transient selector gap, not a release request;
	// Cleanup handles actual deletion.
	if len(nodes) == 0 {
		logger.Info("no router nodes matched; leaving Route Server peerings as they are")
		return nil
	}

	vms, err := toVirtualMachines(nodes)
	if err != nil {
		return err
	}

	// Forwarding first. A peering to a node that drops forwarded packets
	// establishes and carries nothing, which is the failure that looks healthy.
	if err := p.ensureNodesCanForward(ctx, vms); err != nil {
		return err
	}

	desired := make([]Peer, 0, len(nodes))
	for _, n := range nodes {
		desired = append(desired, Peer{
			Name:    peeringName(p.cfg.ClusterID, n.PrivateIP),
			PeerIP:  n.PrivateIP,
			PeerASN: p.cfg.LocalASN,
		})
	}

	changed, err := p.reconcileOurPeerings(ctx, desired)
	if err != nil {
		return fmt.Errorf("reconciling Route Server peerings: %w", err)
	}
	if changed {
		logger.Info("Route Server peerings updated",
			"routeServer", p.cfg.RouteServerName, "nodes", len(nodes))
	}
	return nil
}

// reconcileOurPeerings writes this cluster's peerings without disturbing any
// others.
//
// ReconcilePeers removes every peering absent from the set it is given, so
// peerings belonging to another cluster are added to that set and survive.
func (p *Platform) reconcileOurPeerings(ctx context.Context, desired []Peer) (bool, error) {
	current, err := p.rs.ListPeers(ctx)
	if err != nil {
		return false, err
	}
	for _, c := range current {
		if !isOurPeering(c.Name, p.cfg.ClusterID) {
			desired = append(desired, c.Peer)
		}
	}
	return p.rs.ReconcilePeers(ctx, desired)
}

// Cleanup removes this cluster's Route Server peerings and nothing else.
//
// Interface forwarding is left enabled, as it is on AWS and GCP.
func (p *Platform) Cleanup(ctx context.Context) error {
	current, err := p.rs.ListPeers(ctx)
	if err != nil {
		return fmt.Errorf("listing Route Server peerings: %w", err)
	}
	keep := make([]Peer, 0, len(current))
	for _, c := range current {
		if !isOurPeering(c.Name, p.cfg.ClusterID) {
			keep = append(keep, c.Peer)
		}
	}
	if _, err := p.rs.ReconcilePeers(ctx, keep); err != nil {
		return fmt.Errorf("removing Route Server peerings: %w", err)
	}
	return nil
}

// toVirtualMachines resolves the cloud-neutral node list into Azure VM
// identities.
func toVirtualMachines(nodes []platform.RouterNode) ([]VirtualMachine, error) {
	out := make([]VirtualMachine, 0, len(nodes))
	for _, n := range nodes {
		vm, err := ParseProviderID(n.ProviderID)
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", n.Name, err)
		}
		out = append(out, vm)
	}
	return out, nil
}
