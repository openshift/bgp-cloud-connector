package platform

import (
	"context"
	"errors"
)

// RouterNode is a cluster node the cloud reconciles.
type RouterNode struct {
	Name      string
	PrivateIP string
	// Zone is the node's failure domain. Only a cloud whose BGP endpoints are
	// per subnet partitions on it, so it is set for every node and read by
	// the clouds that need it.
	Zone       string
	ProviderID string
	// ForwardingOnly marks a node that is not a BGP speaker.
	ForwardingOnly bool
}

// Speakers returns the nodes that are BGP speakers.
func Speakers(nodes []RouterNode) []RouterNode {
	out := make([]RouterNode, 0, len(nodes))
	for _, n := range nodes {
		if n.ForwardingOnly {
			continue
		}
		out = append(out, n)
	}
	return out
}

// DiscoveredNeighbor is one address the router nodes peer with.
type DiscoveredNeighbor struct {
	Address string
	ASN     int64
	// EBGPMultiHop is set when the neighbour is not on the node's link, which
	// is a property of the cloud's topology rather than anything this operator
	// chooses.
	//
	EBGPMultiHop bool
}

// PeerGroup is a set of router nodes sharing a neighbour set. Each becomes one
// FRRConfiguration.
type PeerGroup struct {
	// Key identifies the group in the cloud's own terms, for diagnostics: an
	// availability zone on AWS, and whatever names the single regional
	// endpoint on a cloud that has one.
	Key string
	// NodeSelector narrows spec.routerNodeSelector to this group's nodes.
	// Empty means every router node, which is what a cloud with one group
	// emits.
	NodeSelector map[string]string
	Neighbors    []DiscoveredNeighbor
	// RawFRRConfig, when non-empty, becomes spec.raw.rawConfig on the
	// generated FRRConfiguration. Some clouds need FRR directives frr-k8s
	// does not express as fields: a GCP Cloud Router interface needs
	// disable-connected-check, which has no structured equivalent. Without
	// this a platform needing one could not be rendered here at all.
	RawFRRConfig string
}

// DiscoveryResult is the peering plan a cloud arrived at.
//
// It carries nothing cloud-specific: the number of groups is where clouds
// differ, not the type. A cloud with per-subnet endpoints emits one group per
// zone, and a cloud presenting one regional pair of addresses emits a single
// group.
type DiscoveryResult struct {
	PeerGroups []PeerGroup
}

// ErrCredentialsPending reports that the cluster has been asked for cloud
// credentials and has not produced them yet. It lives beside
// CredentialError for the same reason, and it is deliberately distinct
// from one: waiting for a mint is not a fault, and the controller waits
// it out rather than reporting the operator degraded.
var ErrCredentialsPending = errors.New("waiting for the cluster to provide cloud credentials")

// CredentialError reports that a platform could not authenticate against its
// cloud. It lives here rather than in one provider's package because the
// controller reports it as a distinct condition reason, and a credential
// failure is not a discovery failure on any cloud.
type CredentialError struct {
	Msg string
}

func (e *CredentialError) Error() string {
	return e.Msg
}

// CloudPlatform is everything the controller needs from a cloud.
type CloudPlatform interface {
	DiscoverEndpoints(ctx context.Context) (*DiscoveryResult, error)
	ReconcileNodes(ctx context.Context, nodes []RouterNode) error
	Cleanup(ctx context.Context) error
}
