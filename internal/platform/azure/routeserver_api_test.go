package azure

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
)

// fakeBgpLister stands in for armnetwork.VirtualHubBgpConnectionsClient. Each
// entry in pages is one page NextPage returns; errAt, if >= 0, fails the
// fetch at that page index instead of returning it.
type fakeBgpLister struct {
	pages [][]*armnetwork.BgpConnection
	errAt int
	err   error
}

func (f *fakeBgpLister) NewListPager(_, _ string, _ *armnetwork.VirtualHubBgpConnectionsClientListOptions) *runtime.Pager[armnetwork.VirtualHubBgpConnectionsClientListResponse] {
	idx := 0
	return runtime.NewPager(runtime.PagingHandler[armnetwork.VirtualHubBgpConnectionsClientListResponse]{
		More: func(armnetwork.VirtualHubBgpConnectionsClientListResponse) bool {
			return idx < len(f.pages) || idx == f.errAt
		},
		Fetcher: func(context.Context, *armnetwork.VirtualHubBgpConnectionsClientListResponse) (armnetwork.VirtualHubBgpConnectionsClientListResponse, error) {
			if idx == f.errAt {
				return armnetwork.VirtualHubBgpConnectionsClientListResponse{}, f.err
			}
			page := f.pages[idx]
			idx++
			return armnetwork.VirtualHubBgpConnectionsClientListResponse{
				ListVirtualHubBgpConnectionResults: armnetwork.ListVirtualHubBgpConnectionResults{Value: page},
			}, nil
		},
	})
}

// fakeBgpMutator stands in for the collapsed create/update/delete calls,
// recording what it was asked to do and optionally refusing by name.
type fakeBgpMutator struct {
	createErr map[string]error
	deleteErr map[string]error

	created []armnetwork.BgpConnection
	deleted []string
}

func (f *fakeBgpMutator) createOrUpdate(_ context.Context, _, _, name string, params armnetwork.BgpConnection) error {
	if err := f.createErr[name]; err != nil {
		return err
	}
	f.created = append(f.created, params)
	return nil
}

func (f *fakeBgpMutator) delete(_ context.Context, _, _, name string) error {
	if err := f.deleteErr[name]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, name)
	return nil
}

func conn(name, ip string, asn int64) *armnetwork.BgpConnection {
	return &armnetwork.BgpConnection{
		Name: to.Ptr(name),
		Properties: &armnetwork.BgpConnectionProperties{
			PeerIP:            to.Ptr(ip),
			PeerAsn:           to.Ptr(asn),
			ProvisioningState: to.Ptr(armnetwork.ProvisioningStateSucceeded),
			ConnectionState:   to.Ptr(armnetwork.HubBgpConnectionStatusConnected),
		},
	}
}

// TestListPeers_MapsFieldsAndSkipsUnnamed pins the field mapping: a
// connection with no name is not a peer we can act on, and one with no
// Properties still comes back rather than panicking.
func TestListPeers_MapsFieldsAndSkipsUnnamed(t *testing.T) {
	b := &RouteServerBackend{ListClient: &fakeBgpLister{errAt: -1, pages: [][]*armnetwork.BgpConnection{{
		nil,
		{Name: nil},
		conn("b-peer", "10.0.0.2", 65002),
		{Name: to.Ptr("bare")},
		conn("a-peer", "10.0.0.1", 65001),
	}}}}

	got, err := b.ListPeers(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d peers, want 3: %+v", len(got), got)
	}
	// Sorted by name.
	if got[0].Name != "a-peer" || got[1].Name != "b-peer" || got[2].Name != "bare" {
		t.Errorf("not sorted by name: %+v", got)
	}
	if got[0].PeerIP != "10.0.0.1" || got[0].PeerASN != 65001 {
		t.Errorf("a-peer fields = %+v", got[0])
	}
	if got[0].ProvisioningState != "Succeeded" || got[0].PeerBGPState != "Connected" {
		t.Errorf("a-peer state fields = %+v", got[0])
	}
	if got[2].PeerIP != "" || got[2].PeerASN != 0 {
		t.Errorf("a connection with no Properties should map to zero values, got %+v", got[2])
	}
}

// TestListPeers_Paginates pins that every page is walked.
func TestListPeers_Paginates(t *testing.T) {
	b := &RouteServerBackend{ListClient: &fakeBgpLister{errAt: -1, pages: [][]*armnetwork.BgpConnection{
		{conn("a", "10.0.0.1", 1)},
		{conn("b", "10.0.0.2", 2)},
	}}}

	got, err := b.ListPeers(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d peers across 2 pages, want 2", len(got))
	}
}

// TestListPeers_Page2Error pins that a failure fetching a later page is
// reported rather than silently truncating the peer list.
func TestListPeers_Page2Error(t *testing.T) {
	wantErr := errors.New("boom")
	b := &RouteServerBackend{ListClient: &fakeBgpLister{
		pages: [][]*armnetwork.BgpConnection{{conn("a", "10.0.0.1", 1)}},
		errAt: 1,
		err:   wantErr,
	}}

	_, err := b.ListPeers(context.Background())
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("got error %v, want it to wrap %v", err, wantErr)
	}
}

// TestReconcilePeers_NoopWhenEqual pins that a Route Server already matching
// desired is left untouched: no create, update, or delete call is made.
func TestReconcilePeers_NoopWhenEqual(t *testing.T) {
	mutator := &fakeBgpMutator{}
	b := &RouteServerBackend{
		ListClient:   &fakeBgpLister{errAt: -1, pages: [][]*armnetwork.BgpConnection{{conn("a", "10.0.0.1", 65001)}}},
		MutateClient: mutator,
	}

	changed, err := b.ReconcilePeers(context.Background(), []Peer{{Name: "a", PeerIP: "10.0.0.1", PeerASN: 65001}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if changed {
		t.Error("expected no change")
	}
	if len(mutator.created) != 0 || len(mutator.deleted) != 0 {
		t.Errorf("expected no API calls, got created=%v deleted=%v", mutator.created, mutator.deleted)
	}
}

// TestReconcilePeers_RewritesFailedPeering pins that a peering Azure left in
// a terminal non-Succeeded state is rewritten rather than accepted.
//
// Observed on 10 September: two concurrent writes to the same Route Server
// were refused with ConflictError, and Azure kept both peerings with their
// name, IP and ASN intact and provisioningState Failed. Comparing only those
// three fields made them indistinguishable from working ones, so the
// reconcile returned early, the operator reported Ready with all six
// conditions True, and two of six BGP sessions stayed down with nothing left
// to repair them.
func TestReconcilePeers_RewritesFailedPeering(t *testing.T) {
	mutator := &fakeBgpMutator{}
	failed := conn("a", "10.0.0.1", 65001)
	failed.Properties.ProvisioningState = to.Ptr(armnetwork.ProvisioningStateFailed)

	b := &RouteServerBackend{
		ListClient:   &fakeBgpLister{errAt: -1, pages: [][]*armnetwork.BgpConnection{{failed}}},
		MutateClient: mutator,
	}

	changed, err := b.ReconcilePeers(context.Background(), []Peer{{Name: "a", PeerIP: "10.0.0.1", PeerASN: 65001}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Error("a Failed peering should be reported as a change")
	}
	if len(mutator.created) != 1 {
		t.Fatalf("got %d create/update calls, want 1: %+v", len(mutator.created), mutator.created)
	}
	if len(mutator.deleted) != 0 {
		t.Errorf("expected no deletes, got %v", mutator.deleted)
	}
}

// TestReconcilePeers_CreatesMissingAndUpdatesChanged covers both reasons a
// peer is written: it is absent, or it is present with the wrong IP/ASN.
func TestReconcilePeers_CreatesMissingAndUpdatesChanged(t *testing.T) {
	mutator := &fakeBgpMutator{}
	b := &RouteServerBackend{
		ListClient: &fakeBgpLister{errAt: -1, pages: [][]*armnetwork.BgpConnection{{
			conn("stale-ip", "10.0.0.9", 65001),
		}}},
		MutateClient: mutator,
	}

	desired := []Peer{
		{Name: "stale-ip", PeerIP: "10.0.0.1", PeerASN: 65001}, // present but wrong IP
		{Name: "new-peer", PeerIP: "10.0.0.2", PeerASN: 65001}, // absent
	}
	changed, err := b.ReconcilePeers(context.Background(), desired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Error("expected a change")
	}
	if len(mutator.created) != 2 {
		t.Fatalf("got %d create/update calls, want 2: %+v", len(mutator.created), mutator.created)
	}
}

// TestReconcilePeers_DeletesStale pins that a peer absent from desired is
// removed.
func TestReconcilePeers_DeletesStale(t *testing.T) {
	mutator := &fakeBgpMutator{}
	b := &RouteServerBackend{
		ListClient: &fakeBgpLister{errAt: -1, pages: [][]*armnetwork.BgpConnection{{
			conn("keep", "10.0.0.1", 65001),
			conn("drop", "10.0.0.2", 65001),
		}}},
		MutateClient: mutator,
	}

	changed, err := b.ReconcilePeers(context.Background(), []Peer{{Name: "keep", PeerIP: "10.0.0.1", PeerASN: 65001}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !changed {
		t.Error("expected a change")
	}
	if len(mutator.deleted) != 1 || mutator.deleted[0] != "drop" {
		t.Errorf("deleted = %v, want [drop]", mutator.deleted)
	}
}

// TestReconcilePeers_PropagatesCreateError pins that a failed create is
// reported rather than swallowed, so a caller does not believe reconciliation
// succeeded.
func TestReconcilePeers_PropagatesCreateError(t *testing.T) {
	wantErr := errors.New("create boom")
	mutator := &fakeBgpMutator{createErr: map[string]error{"new-peer": wantErr}}
	b := &RouteServerBackend{
		ListClient:   &fakeBgpLister{errAt: -1, pages: [][]*armnetwork.BgpConnection{{}}},
		MutateClient: mutator,
	}

	_, err := b.ReconcilePeers(context.Background(), []Peer{{Name: "new-peer", PeerIP: "10.0.0.2", PeerASN: 65001}})
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("got error %v, want it to wrap %v", err, wantErr)
	}
}

// TestDeleteAllPeers_DeletesEveryPeerAndPropagatesError covers both the
// happy path and a failure partway through.
func TestDeleteAllPeers_DeletesEveryPeerAndPropagatesError(t *testing.T) {
	mutator := &fakeBgpMutator{}
	b := &RouteServerBackend{
		ListClient: &fakeBgpLister{errAt: -1, pages: [][]*armnetwork.BgpConnection{{
			conn("a", "10.0.0.1", 1),
			conn("b", "10.0.0.2", 2),
		}}},
		MutateClient: mutator,
	}
	if err := b.DeleteAllPeers(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mutator.deleted) != 2 {
		t.Fatalf("deleted = %v, want both peers removed", mutator.deleted)
	}

	wantErr := errors.New("delete boom")
	mutator2 := &fakeBgpMutator{deleteErr: map[string]error{"a": wantErr}}
	b.MutateClient = mutator2
	if err := b.DeleteAllPeers(context.Background()); err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("got error %v, want it to wrap %v", err, wantErr)
	}
}
