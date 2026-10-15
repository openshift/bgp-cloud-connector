package gcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// NewComputeClient builds a ComputeClient using Application Default Credentials.
func NewComputeClient(ctx context.Context, project, region string) (ComputeClient, error) {
	svc, err := compute.NewService(ctx, option.WithScopes(compute.CloudPlatformScope))
	if err != nil {
		return nil, err
	}
	return &computeClient{api: &gceAPI{
		svc:     svc,
		project: project,
		region:  region,
		inst:    svc.Instances,
		routers: svc.Routers,
	}}, nil
}

// computeAPI is the slice of the GCE API computeClient needs, as an interface a
// test can fake in place of the live service. Each method is one SDK round-trip;
// the reconcile logic that decides which to call lives above it on computeClient.
type computeAPI interface {
	GetInstance(ctx context.Context, zone, name string) (*compute.Instance, error)
	UpdateInstance(ctx context.Context, zone, name string, inst *compute.Instance, mostDisruptiveAllowedAction string) (*compute.Operation, error)
	GetRouter(ctx context.Context, name string) (*compute.Router, error)
	PatchRouter(ctx context.Context, name string, patch *compute.Router) (*compute.Operation, error)
	UpdateRouter(ctx context.Context, name string, router *compute.Router) (*compute.Operation, error)
	WaitZoneOp(ctx context.Context, zone string, op *compute.Operation) error
	WaitRegionOp(ctx context.Context, op *compute.Operation) error
}

type computeClient struct {
	api computeAPI
}

func (c *computeClient) EnsureCanIPForward(ctx context.Context, node RouterNode) (bool, error) {
	zone := shortZone(node.Zone)
	inst, err := c.api.GetInstance(ctx, zone, node.Name)
	if err != nil {
		return false, err
	}
	if inst.CanIpForward {
		return false, nil
	}
	inst.CanIpForward = true
	op, err := c.api.UpdateInstance(ctx, zone, node.Name, inst, "REFRESH")
	if err != nil {
		return false, err
	}
	if err := c.api.WaitZoneOp(ctx, zone, op); err != nil {
		return false, err
	}
	return true, nil
}

func (c *computeClient) EnsureNestedVirtualization(ctx context.Context, node RouterNode) (bool, error) {
	zone := shortZone(node.Zone)
	inst, err := c.api.GetInstance(ctx, zone, node.Name)
	if err != nil {
		return false, err
	}
	if inst.AdvancedMachineFeatures != nil && inst.AdvancedMachineFeatures.EnableNestedVirtualization {
		return false, nil
	}
	if inst.AdvancedMachineFeatures == nil {
		inst.AdvancedMachineFeatures = &compute.AdvancedMachineFeatures{}
	}
	inst.AdvancedMachineFeatures.EnableNestedVirtualization = true
	// GCP rejects REFRESH for this field; API returns 400 requiring RESTART.
	op, err := c.api.UpdateInstance(ctx, zone, node.Name, inst, "RESTART")
	if err != nil {
		return false, err
	}
	if err := c.api.WaitZoneOp(ctx, zone, op); err != nil {
		return false, err
	}
	return true, nil
}

func (c *computeClient) GetRouterTopology(ctx context.Context, routerName string) (*CloudRouterTopology, error) {
	r, err := c.api.GetRouter(ctx, routerName)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(r.Interfaces))
	ips := make([]string, 0, len(r.Interfaces))
	for _, iface := range r.Interfaces {
		names = append(names, iface.Name)
		ip := iface.IpRange
		if idx := strings.Index(ip, "/"); idx >= 0 {
			ip = ip[:idx]
		}
		ips = append(ips, ip)
	}
	var asn int64
	if r.Bgp != nil {
		asn = r.Bgp.Asn
	}
	return &CloudRouterTopology{
		CloudRouterASN: asn,
		InterfaceNames: names,
		InterfaceIPs:   ips,
	}, nil
}

func (c *computeClient) ReconcilePeers(ctx context.Context, routerName, clusterName string, nodes []RouterNode, topology *CloudRouterTopology, frrASN int) (bool, error) {
	r, err := c.api.GetRouter(ctx, routerName)
	if err != nil {
		return false, err
	}

	desired := mergePeers(r.BgpPeers, desiredPeers(clusterName, nodes, topology, frrASN), clusterName)

	currentSet := buildPeerSet(r.BgpPeers)
	desiredSet := buildPeerSet(desired)
	if currentSet.Equal(desiredSet) {
		return false, nil
	}

	patch := &compute.Router{BgpPeers: desired}
	op, err := c.api.PatchRouter(ctx, routerName, patch)
	if err != nil {
		return false, err
	}
	if err := c.api.WaitRegionOp(ctx, op); err != nil {
		return false, err
	}
	return true, nil
}

func (c *computeClient) ClearPeers(ctx context.Context, routerName, clusterName string) (bool, error) {
	r, err := c.api.GetRouter(ctx, routerName)
	if err != nil {
		return false, err
	}
	kept := mergePeers(r.BgpPeers, nil, clusterName)
	if len(kept) == len(r.BgpPeers) {
		return false, nil
	}
	r.BgpPeers = kept
	op, err := c.api.UpdateRouter(ctx, routerName, r)
	if err != nil {
		if ge, ok := err.(*googleapi.Error); ok && ge.Code == 400 {
			return false, err
		}
		return false, err
	}
	if err := c.api.WaitRegionOp(ctx, op); err != nil {
		return false, err
	}
	return true, nil
}

// gceAPI is the live computeAPI: a thin pass-through to the generated GCE
// client that holds no logic worth unit testing on its own. The project and
// region every call needs live here, out of the reconcile logic above.
type gceAPI struct {
	svc     *compute.Service
	project string
	region  string
	inst    *compute.InstancesService
	routers *compute.RoutersService
}

func (g *gceAPI) GetInstance(ctx context.Context, zone, name string) (*compute.Instance, error) {
	return g.inst.Get(g.project, zone, name).Context(ctx).Do()
}

func (g *gceAPI) UpdateInstance(ctx context.Context, zone, name string, inst *compute.Instance, mostDisruptiveAllowedAction string) (*compute.Operation, error) {
	return g.inst.Update(g.project, zone, name, inst).
		MostDisruptiveAllowedAction(mostDisruptiveAllowedAction).
		Context(ctx).
		Do()
}

func (g *gceAPI) GetRouter(ctx context.Context, name string) (*compute.Router, error) {
	return g.routers.Get(g.project, g.region, name).Context(ctx).Do()
}

func (g *gceAPI) PatchRouter(ctx context.Context, name string, patch *compute.Router) (*compute.Operation, error) {
	return g.routers.Patch(g.project, g.region, name, patch).Context(ctx).Do()
}

func (g *gceAPI) UpdateRouter(ctx context.Context, name string, router *compute.Router) (*compute.Operation, error) {
	return g.routers.Update(g.project, g.region, name, router).Context(ctx).Do()
}

func (g *gceAPI) WaitZoneOp(ctx context.Context, zone string, op *compute.Operation) error {
	if op == nil {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		cur, err := g.svc.ZoneOperations.Get(g.project, zone, op.Name).Context(ctx).Do()
		if err != nil {
			return err
		}
		if done, err := computeOpDone(cur); done {
			return err
		}
	}
}

func (g *gceAPI) WaitRegionOp(ctx context.Context, op *compute.Operation) error {
	if op == nil {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		cur, err := g.svc.RegionOperations.Get(g.project, g.region, op.Name).Context(ctx).Do()
		if err != nil {
			return err
		}
		if done, err := computeOpDone(cur); done {
			return err
		}
	}
}

// computeOpDone reports whether a GCE operation has reached its terminal state,
// and if so whether it failed. A zone and a region operation are polled from
// different endpoints but carry the same status and error shape, so both wait
// loops decide when to stop here. Splitting this out lets the DONE-with-error
// path be tested without a fake clock behind the WaitZoneOp/WaitRegionOp seam.
func computeOpDone(op *compute.Operation) (bool, error) {
	if op.Status != "DONE" {
		return false, nil
	}
	if op.Error != nil {
		return true, fmt.Errorf("operation failed: %v", op.Error)
	}
	return true, nil
}

// maxPeerNameLength is the GCE limit on a Cloud Router peer name.
const maxPeerNameLength = 63

// maxPeerPrefixLength bounds the ownership prefix, and peerDigestLength is how
// much of a SHA-256 is kept wherever something has to be abbreviated to fit.
//
// The bound sits well above an OpenShift infrastructure name plus "-bgp", so
// in practice no prefix is abbreviated at all. It exists so that the limit is
// enforced on the prefix alone, before any address is appended: a prefix
// trimmed to fit a particular address is a prefix isOurPeer cannot look for.
const (
	maxPeerPrefixLength = 34
	peerDigestLength    = 8
)

// peerPrefix is the ownership signal. A Cloud Router peer is a field inside
// the router resource and cannot carry labels, so unlike the AWS tag this is
// the only marker available, and it is why nothing here touches a peer whose
// name does not carry it.
//
// Every name is built from this and isOurPeer looks for exactly this, so the
// two cannot fall out of step. Abbreviating happens here or not at all.
func peerPrefix(clusterName string) string {
	prefix := clusterName + "-bgp"
	if len(prefix) <= maxPeerPrefixLength {
		return prefix
	}
	// Cutting alone would give two clusters sharing their leading characters
	// the same prefix, and each would then treat the other's peers as its own.
	// What is kept of the name is qualified by a digest of the whole of it.
	return prefix[:maxPeerPrefixLength-peerDigestLength-1] + "-" + peerDigest(clusterName)
}

// peerDigest is a short, stable stand-in for a value too long to spell out.
func peerDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:peerDigestLength]
}

// PeerName is the Cloud Router peer name for a node address and router
// interface.
//
// The address is the key rather than the node's position in the list. Naming
// peers positionally means one node leaving renumbers every peer after it,
// and since a patch replaces the whole list that is a delete and a create:
// every surviving node's session drops because an unrelated node went away.
func PeerName(clusterName, ipAddress string, ifaceIdx int) string {
	prefix := peerPrefix(clusterName)

	name := fmt.Sprintf("%s-%s-%d", prefix, addressToken(ipAddress), ifaceIdx)
	if len(name) <= maxPeerNameLength {
		return name
	}
	// The prefix is what marks the peer as ours, so when something has to go
	// it is the address, which a digest still tells apart node by node.
	return fmt.Sprintf("%s-%s-%d", prefix, peerDigest(ipAddress), ifaceIdx)
}

// addressToken renders a node address as part of a GCE name, which accepts
// only lowercase letters, digits and dashes.
//
// Substituting every separator rather than just the IPv4 dot is what makes an
// IPv6 address usable here: a colon is not a name character, and a name
// carrying one is rejected outright, so a dual-stack node whose first internal
// address is IPv6 got no peer at all. Every separator maps to the same dash,
// which is safe because an address cannot mix the two families.
func addressToken(ipAddress string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		default:
			return '-'
		}
	}, ipAddress)
}

// isOurPeer reports whether a peer name was generated for this cluster. The
// trailing separator matters: without it a cluster named "cluster" would claim
// the peers of one named "cluster-two".
func isOurPeer(name, clusterName string) bool {
	return strings.HasPrefix(name, peerPrefix(clusterName)+"-")
}

// desiredPeers builds one peer per node and router interface, ordered by name
// so an unchanged node set produces an identical list.
func desiredPeers(clusterName string, nodes []RouterNode, topology *CloudRouterTopology, frrASN int) []*compute.RouterBgpPeer {
	var peers []*compute.RouterBgpPeer
	for _, node := range nodes {
		for ifaceIdx, ifaceName := range topology.InterfaceNames {
			if ifaceIdx >= len(topology.InterfaceIPs) {
				break
			}
			peers = append(peers, &compute.RouterBgpPeer{
				Name:                    PeerName(clusterName, node.IPAddress, ifaceIdx),
				InterfaceName:           ifaceName,
				PeerIpAddress:           node.IPAddress,
				IpAddress:               topology.InterfaceIPs[ifaceIdx],
				PeerAsn:                 int64(frrASN),
				RouterApplianceInstance: node.SelfLink,
			})
		}
	}
	sortPeers(peers)
	return peers
}

// mergePeers returns the peer list to write: every peer that is not ours, left
// exactly as found, plus our desired set. Routers.Patch uses JSON merge patch,
// so the array is replaced wholesale and anything omitted here is deleted,
// including peers belonging to another cluster sharing the router.
func mergePeers(existing, desired []*compute.RouterBgpPeer, clusterName string) []*compute.RouterBgpPeer {
	out := make([]*compute.RouterBgpPeer, 0, len(existing)+len(desired))
	for _, p := range existing {
		if p != nil && !isOurPeer(p.Name, clusterName) {
			out = append(out, p)
		}
	}
	out = append(out, desired...)
	sortPeers(out)
	return out
}

func sortPeers(peers []*compute.RouterBgpPeer) {
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
}

// peerKey carries every field desiredPeers writes, so that the comparison
// answers the question ReconcilePeers is really asking: would writing the
// desired list change the router?
//
// The name is keyed on the node address and the interface index, so an
// interface rebuilt under the same index, or an instance reissued at the same
// address, produces an identical name. Keying only on the name and the node
// end of the session would call those unchanged and send no patch, leaving the
// peer pointing at an interface or an instance that no longer exists.
type peerKey struct {
	name, ifaceName, peerIP, ifaceIP, applianceInstance string
	peerASN                                             int64
}

type peerSet map[peerKey]struct{}

func buildPeerSet(peers []*compute.RouterBgpPeer) peerSet {
	s := make(peerSet)
	for _, p := range peers {
		if p == nil {
			continue
		}
		s[peerKey{
			name:              p.Name,
			ifaceName:         p.InterfaceName,
			peerIP:            p.PeerIpAddress,
			ifaceIP:           p.IpAddress,
			applianceInstance: p.RouterApplianceInstance,
			peerASN:           p.PeerAsn,
		}] = struct{}{}
	}
	return s
}

func (a peerSet) Equal(b peerSet) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func shortZone(zone string) string {
	if i := strings.LastIndex(zone, "/"); i >= 0 && i+1 < len(zone) {
		return zone[i+1:]
	}
	return zone
}
