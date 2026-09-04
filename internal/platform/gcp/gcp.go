package gcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

// Config is everything the GCP platform needs that the controller cannot work
// out for itself. It is spec.gcp plus the two values every platform is given.
type Config struct {
	Project         string
	Region          string
	CloudRouterName string
	NCCHubName      string
	NCCSpokePrefix  string
	SiteToSite      bool
	NestedVirt      bool
	// LocalASN is the ASN FRR runs with on the router nodes, which is what
	// Cloud Router peers are configured to expect.
	LocalASN int64
	// ClusterID names Cloud Router peers so that two clusters sharing a
	// router do not collide.
	ClusterID string
}

// Platform reconciles GCP Cloud Router peering and NCC spokes for the
// BGP router nodes.
type Platform struct {
	cfg     Config
	compute ComputeClient
	ncc     NCCClient
}

// New builds a Platform against the live Google APIs.
func New(ctx context.Context, cfg Config) (*Platform, error) {
	computeClient, err := NewComputeClient(ctx, cfg.Project, cfg.Region)
	if err != nil {
		return nil, &platform.CredentialError{Msg: fmt.Sprintf("GCP compute client: %v", err)}
	}
	nccClient, err := NewNCCClient(ctx, cfg.Project, cfg.Region)
	if err != nil {
		return nil, &platform.CredentialError{Msg: fmt.Sprintf("GCP network connectivity client: %v", err)}
	}
	return &Platform{cfg: cfg, compute: computeClient, ncc: nccClient}, nil
}

// DiscoverEndpoints reads the Cloud Router and returns a single peer group.
//
// Every router node peers with the same Cloud Router interface addresses, so
// unlike AWS there is no per-zone split and one FRRConfiguration covers every
// router node. The group carries no node selector for the same reason.
func (p *Platform) DiscoverEndpoints(ctx context.Context) (*platform.DiscoveryResult, error) {
	topology, err := p.compute.GetRouterTopology(ctx, p.cfg.CloudRouterName)
	if err != nil {
		return nil, fmt.Errorf("reading Cloud Router %q topology: %w", p.cfg.CloudRouterName, err)
	}
	if len(topology.InterfaceIPs) == 0 {
		return nil, fmt.Errorf("no interfaces on Cloud Router %q", p.cfg.CloudRouterName)
	}

	group := platform.PeerGroup{
		Key:          p.cfg.CloudRouterName,
		RawFRRConfig: rawFRRConfig(p.cfg.LocalASN, topology.InterfaceIPs),
	}
	for _, ip := range topology.InterfaceIPs {
		group.Neighbors = append(group.Neighbors, platform.DiscoveredNeighbor{
			Address: ip,
			ASN:     topology.CloudRouterASN,
		})
	}

	return &platform.DiscoveryResult{PeerGroups: []platform.PeerGroup{group}}, nil
}

// rawFRRConfig renders the FRR directives the structured neighbor API cannot
// express. A Cloud Router interface is not on the node's link in the way FRR
// expects, so without disable-connected-check it rejects the neighbor as
// unreachable and no session comes up.
func rawFRRConfig(localASN int64, interfaceIPs []string) string {
	lines := []string{"      router bgp " + strconv.FormatInt(localASN, 10)}
	for _, ip := range interfaceIPs {
		lines = append(lines, "       neighbor "+ip+" disable-connected-check")
	}
	return strings.Join(lines, "\n") + "\n"
}

// ReconcileNodes brings GCE instance attributes, NCC spokes and Cloud Router
// peers into line with the given router nodes.
func (p *Platform) ReconcileNodes(ctx context.Context, nodes []platform.RouterNode) error {
	logger := log.FromContext(ctx)

	// No router nodes is a gap in the selector, not an instruction to release
	// the estate. Reconciling it deletes every spoke and writes a peer list
	// with none of ours in it, so a label being moved during a rollout would
	// drop BGP for the whole cluster. Cleanup releases these resources, and it
	// runs on deletion, where the intent is unambiguous.
	if len(nodes) == 0 {
		logger.Info("no router nodes matched; leaving Cloud Router peers and NCC spokes as they are")
		return nil
	}

	routerNodes, err := toRouterNodes(nodes)
	if err != nil {
		return err
	}

	for _, node := range routerNodes {
		changed, err := p.compute.EnsureCanIPForward(ctx, node)
		if err != nil {
			return fmt.Errorf("enabling IP forwarding on %q: %w", node.Name, err)
		}
		if changed {
			logger.Info("enabled canIpForward", "instance", node.Name)
		}

		if p.cfg.NestedVirt {
			changed, err := p.compute.EnsureNestedVirtualization(ctx, node)
			if err != nil {
				return fmt.Errorf("enabling nested virtualization on %q: %w", node.Name, err)
			}
			if changed {
				logger.Info("enabled nested virtualization", "instance", node.Name)
			}
		}
	}

	// A VM cannot peer with a Cloud Router until it belongs to a router
	// appliance spoke, so the spokes come before the peers.
	spokeChanges, err := ReconcileNCCSpokes(ctx, p.ncc, p.cfg.NCCHubName, p.cfg.NCCSpokePrefix, p.cfg.SiteToSite, routerNodes)
	if err != nil {
		return fmt.Errorf("reconciling NCC spokes: %w", err)
	}
	if spokeChanges > 0 {
		logger.Info("NCC spokes updated", "changes", spokeChanges)
	}

	topology, err := p.compute.GetRouterTopology(ctx, p.cfg.CloudRouterName)
	if err != nil {
		return fmt.Errorf("reading Cloud Router %q topology: %w", p.cfg.CloudRouterName, err)
	}
	changed, err := p.compute.ReconcilePeers(ctx, p.cfg.CloudRouterName, p.cfg.ClusterID, routerNodes, topology, int(p.cfg.LocalASN))
	if err != nil {
		return fmt.Errorf("reconciling Cloud Router peers: %w", err)
	}
	if changed {
		logger.Info("Cloud Router peers updated", "router", p.cfg.CloudRouterName, "nodes", len(routerNodes))
	}
	return nil
}

// Cleanup releases the Cloud Router peers and NCC spokes this platform made.
func (p *Platform) Cleanup(ctx context.Context) error {
	if _, err := p.compute.ClearPeers(ctx, p.cfg.CloudRouterName, p.cfg.ClusterID); err != nil {
		return fmt.Errorf("clearing Cloud Router peers: %w", err)
	}
	ids, err := p.ncc.ListSpokesByPrefix(ctx, p.cfg.NCCHubName, p.cfg.NCCSpokePrefix)
	if err != nil {
		return fmt.Errorf("listing NCC spokes: %w", err)
	}
	for _, id := range ids {
		if _, err := p.ncc.DeleteSpoke(ctx, id); err != nil {
			return fmt.Errorf("deleting NCC spoke %q: %w", id, err)
		}
	}
	return nil
}

// toRouterNodes resolves the cloud-neutral node list into GCE identities.
func toRouterNodes(nodes []platform.RouterNode) ([]RouterNode, error) {
	out := make([]RouterNode, 0, len(nodes))
	for _, n := range nodes {
		inst, err := ParseProviderID(n.ProviderID)
		if err != nil {
			return nil, fmt.Errorf("node %q: %w", n.Name, err)
		}
		out = append(out, RouterNode{
			Name:      inst.Name,
			SelfLink:  inst.SelfLink,
			Zone:      inst.Zone,
			IPAddress: n.PrivateIP,
		})
	}
	return out, nil
}
