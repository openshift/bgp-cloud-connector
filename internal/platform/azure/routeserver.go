package azure

import (
	"context"
	"fmt"
	"sort"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// bgpListerAPI is the subset of armnetwork.VirtualHubBgpConnectionsClient this
// package calls.
type bgpListerAPI interface {
	NewListPager(resourceGroupName, virtualHubName string, options *armnetwork.VirtualHubBgpConnectionsClientListOptions) *runtime.Pager[armnetwork.VirtualHubBgpConnectionsClientListResponse]
}

// bgpMutatorAPI collapses armnetwork.VirtualHubBgpConnectionClient's
// Begin+Poll create/update/delete calls into synchronous ones; see
// awaitPoller.
type bgpMutatorAPI interface {
	createOrUpdate(ctx context.Context, resourceGroup, routeServerName, name string, params armnetwork.BgpConnection) error
	delete(ctx context.Context, resourceGroup, routeServerName, name string) error
}

// awaitPoller collapses a Begin+Poll pair into one call: no fake can produce
// the concrete Poller[T] Begin returns, and nothing here needs it mid-flight.
// The failing phase (begin vs poll) stays in the error text for triage.
func awaitPoller[T any](ctx context.Context, poller *runtime.Poller[T], beginErr error) error {
	if beginErr != nil {
		return fmt.Errorf("begin: %w", beginErr)
	}
	if _, err := poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("poll: %w", err)
	}
	return nil
}

type bgpConnectionMutator struct {
	client *armnetwork.VirtualHubBgpConnectionClient
}

func (m *bgpConnectionMutator) createOrUpdate(ctx context.Context, resourceGroup, routeServerName, name string, params armnetwork.BgpConnection) error {
	poller, err := m.client.BeginCreateOrUpdate(ctx, resourceGroup, routeServerName, name, params, nil)
	return awaitPoller(ctx, poller, err)
}

func (m *bgpConnectionMutator) delete(ctx context.Context, resourceGroup, routeServerName, name string) error {
	poller, err := m.client.BeginDelete(ctx, resourceGroup, routeServerName, name, nil)
	return awaitPoller(ctx, poller, err)
}

// RouteServerBackend manages Azure Route Server BGP peerings.
type RouteServerBackend struct {
	ResourceGroup   string
	RouteServerName string
	ListClient      bgpListerAPI
	MutateClient    bgpMutatorAPI
}

type peerKey struct {
	name    string
	peerIP  string
	peerASN int64
}

type peerSet map[peerKey]struct{}

// NewRouteServerBackend builds a backend for a Route Server.
func NewRouteServerBackend(subscriptionID, resourceGroup, routeServerName string, cred azcore.TokenCredential) (*RouteServerBackend, error) {
	factory, err := armnetwork.NewClientFactory(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("azure network client factory: %w", err)
	}
	return &RouteServerBackend{
		ResourceGroup:   resourceGroup,
		RouteServerName: routeServerName,
		ListClient:      factory.NewVirtualHubBgpConnectionsClient(),
		MutateClient:    &bgpConnectionMutator{client: factory.NewVirtualHubBgpConnectionClient()},
	}, nil
}

// ListPeers returns current BGP connections on the Route Server.
func (b *RouteServerBackend) ListPeers(ctx context.Context) ([]ObservedPeer, error) {
	var out []ObservedPeer
	pager := b.ListClient.NewListPager(b.ResourceGroup, b.RouteServerName, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list route server peerings: %w", err)
		}
		for _, conn := range page.Value {
			if conn == nil || conn.Name == nil {
				continue
			}
			peer := ObservedPeer{
				Peer: Peer{
					Name: *conn.Name,
				},
			}
			if conn.Properties != nil {
				if conn.Properties.PeerIP != nil {
					peer.PeerIP = *conn.Properties.PeerIP
				}
				if conn.Properties.PeerAsn != nil {
					peer.PeerASN = *conn.Properties.PeerAsn
				}
				if conn.Properties.ProvisioningState != nil {
					peer.ProvisioningState = string(*conn.Properties.ProvisioningState)
				}
				if conn.Properties.ConnectionState != nil {
					peer.PeerBGPState = string(*conn.Properties.ConnectionState)
				}
			}
			out = append(out, peer)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ReconcilePeers creates, updates, or deletes peerings to match desired.
func (b *RouteServerBackend) ReconcilePeers(ctx context.Context, desired []Peer) (bool, error) {
	log := logf.FromContext(ctx)

	current, err := b.ListPeers(ctx)
	if err != nil {
		return false, err
	}

	desiredSorted := append([]Peer(nil), desired...)
	sort.Slice(desiredSorted, func(i, j int) bool { return desiredSorted[i].Name < desiredSorted[j].Name })

	if buildPeerSet(current).Equal(buildDesiredSet(desiredSorted)) {
		return false, nil
	}

	log.Info("reconciling Azure Route Server BGP peerings",
		"resourceGroup", b.ResourceGroup,
		"routeServer", b.RouteServerName,
		"currentPeerCount", len(current),
		"desiredPeerCount", len(desiredSorted),
	)

	desiredByName := make(map[string]Peer, len(desiredSorted))
	for _, p := range desiredSorted {
		desiredByName[p.Name] = p
	}

	changed := false
	for name, want := range desiredByName {
		cur, ok := findCurrent(current, name)
		if !ok || !cur.applied() || cur.PeerIP != want.PeerIP || cur.PeerASN != want.PeerASN {
			if err := b.createOrUpdate(ctx, want); err != nil {
				return changed, err
			}
			changed = true
		}
	}

	for _, cur := range current {
		if _, keep := desiredByName[cur.Name]; keep {
			continue
		}
		if err := b.delete(ctx, cur.Name); err != nil {
			return changed, err
		}
		changed = true
	}

	return changed, nil
}

// DeleteAllPeers removes all BGP connections on the Route Server.
func (b *RouteServerBackend) DeleteAllPeers(ctx context.Context) error {
	log := logf.FromContext(ctx)

	current, err := b.ListPeers(ctx)
	if err != nil {
		return err
	}
	if len(current) == 0 {
		return nil
	}
	log.Info("deleting all Azure Route Server BGP peerings",
		"resourceGroup", b.ResourceGroup,
		"routeServer", b.RouteServerName,
		"peerCount", len(current),
	)
	for _, p := range current {
		if err := b.delete(ctx, p.Name); err != nil {
			return err
		}
	}
	return nil
}

func (b *RouteServerBackend) createOrUpdate(ctx context.Context, peer Peer) error {
	log := logf.FromContext(ctx)
	log.Info("calling Azure API to create or update Route Server BGP peering",
		"resourceGroup", b.ResourceGroup,
		"routeServer", b.RouteServerName,
		"peeringName", peer.Name,
		"peerIP", peer.PeerIP,
		"peerASN", peer.PeerASN,
	)
	params := armnetwork.BgpConnection{
		Properties: &armnetwork.BgpConnectionProperties{
			PeerAsn: to.Ptr(peer.PeerASN),
			PeerIP:  to.Ptr(peer.PeerIP),
		},
	}
	if err := b.MutateClient.createOrUpdate(ctx, b.ResourceGroup, b.RouteServerName, peer.Name, params); err != nil {
		return fmt.Errorf("create/update peering %q: %w", peer.Name, err)
	}
	log.Info("Azure Route Server BGP peering create or update completed",
		"resourceGroup", b.ResourceGroup,
		"routeServer", b.RouteServerName,
		"peeringName", peer.Name,
		"peerIP", peer.PeerIP,
		"peerASN", peer.PeerASN,
	)
	return nil
}

func (b *RouteServerBackend) delete(ctx context.Context, name string) error {
	log := logf.FromContext(ctx)
	log.Info("calling Azure API to delete Route Server BGP peering",
		"resourceGroup", b.ResourceGroup,
		"routeServer", b.RouteServerName,
		"peeringName", name,
	)
	if err := b.MutateClient.delete(ctx, b.ResourceGroup, b.RouteServerName, name); err != nil {
		return fmt.Errorf("delete peering %q: %w", name, err)
	}
	log.Info("Azure Route Server BGP peering delete completed",
		"resourceGroup", b.ResourceGroup,
		"routeServer", b.RouteServerName,
		"peeringName", name,
	)
	return nil
}

func findCurrent(current []ObservedPeer, name string) (ObservedPeer, bool) {
	for _, p := range current {
		if p.Name == name {
			return p, true
		}
	}
	return ObservedPeer{}, false
}

func buildDesiredSet(peers []Peer) peerSet {
	s := make(peerSet, len(peers))
	for _, p := range peers {
		s[peerKey{p.Name, p.PeerIP, p.PeerASN}] = struct{}{}
	}
	return s
}

// applied reports whether Azure actually has this peering in place.
//
// A write Azure refuses does not remove the peering: it keeps the name, the
// IP and the ASN and records provisioningState Failed. Observed on 10
// September, where two concurrent writes to one Route Server were rejected
// with ConflictError and left exactly that. Succeeded is the only state that
// means the peering is there, so every other one is treated as absent and
// rewritten -- including the transient ones, where a rewrite Azure rejects
// fails this reconcile and is retried on the next, which is the behaviour we
// want when something else is changing our peerings underneath us.
func (p ObservedPeer) applied() bool {
	return p.ProvisioningState == string(armnetwork.ProvisioningStateSucceeded)
}

// buildPeerSet keys the peerings Azure has actually applied. One it has not
// is left out, so the comparison against desired reports a difference and
// the reconcile below rewrites it.
func buildPeerSet(peers []ObservedPeer) peerSet {
	s := make(peerSet, len(peers))
	for _, p := range peers {
		if !p.applied() {
			continue
		}
		s[peerKey{p.Name, p.PeerIP, p.PeerASN}] = struct{}{}
	}
	return s
}

func (s peerSet) Equal(other peerSet) bool {
	if len(s) != len(other) {
		return false
	}
	for k := range s {
		if _, ok := other[k]; !ok {
			return false
		}
	}
	return true
}
