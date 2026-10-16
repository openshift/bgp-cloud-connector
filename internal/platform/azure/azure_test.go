package azure

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"

	"github.com/openshift/bgp-cloud-connector/internal/platform"
)

const testProviderID = "azure:///subscriptions/53b8f551-f0fc-4bea-8cba-6d1fefd54c8a/resourceGroups/amcdermo-rg/providers/Microsoft.Compute/virtualMachines/worker-centralus1-xjf5l"

func TestParseProviderID(t *testing.T) {
	vm, err := ParseProviderID(testProviderID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vm.ResourceGroup != "amcdermo-rg" {
		t.Errorf("resource group = %q", vm.ResourceGroup)
	}
	if vm.Name != "worker-centralus1-xjf5l" {
		t.Errorf("name = %q", vm.Name)
	}
	// The ARM ID is what a network interface records, so it must come back
	// without the azure:// scheme the provider ID carries.
	want := "/subscriptions/53b8f551-f0fc-4bea-8cba-6d1fefd54c8a/resourceGroups/amcdermo-rg/providers/Microsoft.Compute/virtualMachines/worker-centralus1-xjf5l"
	if vm.ID != want {
		t.Errorf("ID = %q, want %q", vm.ID, want)
	}

	for _, bad := range []string{
		"gce://openshift-qe/us-east1-b/worker",
		"aws:///us-east-2a/i-0123",
		"azure:///subscriptions/sub/resourceGroups/rg/providers/Microsoft.Compute/disks/d1",
		"",
	} {
		if _, err := ParseProviderID(bad); err == nil {
			t.Errorf("%q was accepted as an Azure VM provider ID", bad)
		}
	}
}

func TestIsOurPeering(t *testing.T) {
	name := peeringName("cluster-two", "10.0.128.4")
	if isOurPeering(name, "cluster") {
		t.Errorf("%q was claimed by cluster %q", name, "cluster")
	}
	if !isOurPeering(name, "cluster-two") {
		t.Errorf("%q was not claimed by its own cluster", name)
	}
}

func TestPeeringName_KeyedOnAddressAndWithinLimit(t *testing.T) {
	// Two nodes, and the name follows the address rather than any ordering.
	a := peeringName("cluster", "10.0.128.4")
	b := peeringName("cluster", "10.0.128.5")
	if a == b {
		t.Fatal("two addresses produced the same peering name")
	}
	if a != peeringName("cluster", "10.0.128.4") {
		t.Error("peering name is not stable for the same address")
	}

	long := peeringName("a-very-long-openshift-infrastructure-name-with-a-suffix-abcdefghijkl", "10.128.128.128")
	if len(long) > maxPeeringNameLength {
		t.Errorf("peering name is %d characters, over the %d limit: %q", len(long), maxPeeringNameLength, long)
	}
}

// --- fakes ---

type fakeRS struct {
	current []ObservedPeer
	desired []Peer
}

func (f *fakeRS) ListPeers(_ context.Context) ([]ObservedPeer, error) { return f.current, nil }
func (f *fakeRS) ReconcilePeers(_ context.Context, desired []Peer) (bool, error) {
	f.desired = desired
	return true, nil
}
func (f *fakeRS) DeleteAllPeers(_ context.Context) error { return nil }

var _ Backend = (*fakeRS)(nil)

type fakeNICs struct {
	nics    []NIC
	enabled []string
	// enableErr, when set, is what every EnableIPForwarding returns.
	enableErr error
}

func (f *fakeNICs) ListNICs(_ context.Context, rg string) ([]NIC, error) {
	var out []NIC
	for _, n := range f.nics {
		if n.ResourceGroup == rg {
			out = append(out, n)
		}
	}
	return out, nil
}

func (f *fakeNICs) EnableIPForwarding(_ context.Context, _, name string) error {
	if f.enableErr != nil {
		return f.enableErr
	}
	f.enabled = append(f.enabled, name)
	return nil
}

type fakeTopo struct{ t *RouteServerTopology }

func (f *fakeTopo) GetTopology(_ context.Context) (*RouteServerTopology, error) { return f.t, nil }

// TestReconcileOurPeerings_LeavesOtherClustersAlone covers a Route Server
// reached by two clusters. ReconcilePeers removes every peering absent from
// the set it is given, so another cluster's have to be in it.
func TestReconcileOurPeerings_LeavesOtherClustersAlone(t *testing.T) {
	rs := &fakeRS{current: []ObservedPeer{
		{Peer: Peer{Name: "other-bgp-10-9-9-9", PeerIP: "10.9.9.9", PeerASN: 65001}},
		{Peer: Peer{Name: "hand-made", PeerIP: "10.9.9.10", PeerASN: 65001}},
		{Peer: Peer{Name: peeringName("cluster", "10.0.128.4"), PeerIP: "10.0.128.4", PeerASN: 65001}},
	}}
	p := &Platform{cfg: Config{ClusterID: "cluster", LocalASN: 65001}, rs: rs}

	ours := []Peer{{Name: peeringName("cluster", "10.0.128.4"), PeerIP: "10.0.128.4", PeerASN: 65001}}
	if _, err := p.reconcileOurPeerings(context.Background(), ours); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := map[string]bool{}
	for _, d := range rs.desired {
		got[d.Name] = true
	}
	for _, want := range []string{"other-bgp-10-9-9-9", "hand-made"} {
		if !got[want] {
			t.Errorf("peering %q belongs to another cluster and would be deleted", want)
		}
	}
	if !got[peeringName("cluster", "10.0.128.4")] {
		t.Error("our own peering was dropped")
	}
}

// TestCleanup_RemovesOnlyOurPeerings is the same property on the way out.
func TestCleanup_RemovesOnlyOurPeerings(t *testing.T) {
	rs := &fakeRS{current: []ObservedPeer{
		{Peer: Peer{Name: "other-bgp-10-9-9-9", PeerIP: "10.9.9.9"}},
		{Peer: Peer{Name: peeringName("cluster", "10.0.128.4"), PeerIP: "10.0.128.4"}},
	}}
	p := &Platform{cfg: Config{ClusterID: "cluster"}, rs: rs}

	if err := p.Cleanup(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rs.desired) != 1 || rs.desired[0].Name != "other-bgp-10-9-9-9" {
		t.Errorf("kept %v, want only the other cluster's peering", rs.desired)
	}
}

// TestEnsureNodesCanForward_MatchesOnVMID pins that the interface is found by
// the VM's ARM ID rather than by a name convention, and that an interface
// already forwarding is left alone.
func TestEnsureNodesCanForward_MatchesOnVMID(t *testing.T) {
	vm, err := ParseProviderID(testProviderID)
	if err != nil {
		t.Fatal(err)
	}

	nics := &fakeNICs{nics: []NIC{
		// Named nothing like the node, since the match is on the VM's ARM
		// resource ID and not on any naming convention.
		{Name: "some-other-nic-name", ResourceGroup: "amcdermo-rg", VMID: vm.ID},
		{Name: "already-on", ResourceGroup: "amcdermo-rg", VMID: "/subscriptions/x/other", IPForwarding: true},
	}}
	p := &Platform{cfg: Config{}, nics: nics}

	if err := p.ensureNodesCanForward(context.Background(), []VirtualMachine{vm}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nics.enabled) != 1 || nics.enabled[0] != "some-other-nic-name" {
		t.Errorf("enabled %v, want the interface attached to the VM", nics.enabled)
	}
}

// TestEnsureNodesCanForward_NoInterface refuses rather than silently doing
// nothing, since a router node whose interface drops forwarded packets looks
// entirely healthy from the cluster.
func TestEnsureNodesCanForward_NoInterface(t *testing.T) {
	vm, _ := ParseProviderID(testProviderID)
	p := &Platform{cfg: Config{}, nics: &fakeNICs{}}
	if err := p.ensureNodesCanForward(context.Background(), []VirtualMachine{vm}); err == nil {
		t.Error("expected an error when no interface is attached to the VM")
	}
}

// azureRefusal is what the SDK returns when Azure answers a write with code:
// a ResponseError carrying the request, whose URL names the subscription.
func azureRefusal(t *testing.T, status int, code string) error {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut,
		"https://management.azure.com/subscriptions/53b8f551-f0fc-4bea-8cba-6d1fefd54c8a/resourceGroups/amcdermo-rg/providers/Microsoft.Network/networkInterfaces/router-nic", nil)
	return runtime.NewResponseErrorWithErrorCode(&http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"` + code + `"}}`)),
		Request:    req,
	}, code)
}

// TestEnsureNodesCanForward_RefusedWriteSaysWhatToDo covers an identity Azure
// will not let write the interface: a role without networkInterfaces/write,
// one missing a join on the subnet or backend pool the interface references,
// or a deny assignment. Each arrives as a 403 with its own code. The condition
// should name the node and what to do, not repeat Azure's response, which
// carries the subscription id.
func TestEnsureNodesCanForward_RefusedWriteSaysWhatToDo(t *testing.T) {
	vm, err := ParseProviderID(testProviderID)
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"AuthorizationFailed", "LinkedAuthorizationFailed", "DenyAssignmentAuthorizationFailed"} {
		t.Run(code, func(t *testing.T) {
			refusal := azureRefusal(t, http.StatusForbidden, code)
			nics := &fakeNICs{
				nics:      []NIC{{Name: "router-nic", ResourceGroup: "amcdermo-rg", VMID: vm.ID}},
				enableErr: refusal,
			}
			p := &Platform{cfg: Config{}, nics: nics}

			err := p.ensureNodesCanForward(context.Background(), []VirtualMachine{vm})

			var refused *forwardingRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("got %v, want a forwardingRefusedError", err)
			}
			if refused.nic != "router-nic" || refused.vm != vm.Name || refused.code != code {
				t.Errorf("got nic=%q vm=%q code=%q, want router-nic, %s, %s",
					refused.nic, refused.vm, refused.code, vm.Name, code)
			}
			want := `IP forwarding is off on network interface "router-nic" of "worker-centralus1-xjf5l", ` +
				`and Azure refused this identity's change to it (` + code + `). Enable IP forwarding ` +
				`on the node pool, or let the identity write the interface and join its subnet and ` +
				`load balancer backend pool`
			if err.Error() != want {
				t.Errorf("message\n got: %s\nwant: %s", err.Error(), want)
			}
			if !errors.Is(err, refusal) {
				t.Error("Azure's error is no longer reachable with errors.Is")
			}
		})
	}
}

// TestEnsureNodesCanForward_OtherWriteFailuresUnchanged pins that only a
// refusal is reworded: anything else, a throttle or a server error, is still
// reported as Azure said it.
func TestEnsureNodesCanForward_OtherWriteFailuresUnchanged(t *testing.T) {
	vm, err := ParseProviderID(testProviderID)
	if err != nil {
		t.Fatal(err)
	}
	failure := azureRefusal(t, http.StatusTooManyRequests, "TooManyRequests")
	nics := &fakeNICs{
		nics:      []NIC{{Name: "router-nic", ResourceGroup: "amcdermo-rg", VMID: vm.ID}},
		enableErr: failure,
	}
	p := &Platform{cfg: Config{}, nics: nics}

	err = p.ensureNodesCanForward(context.Background(), []VirtualMachine{vm})

	var refused *forwardingRefusedError
	if errors.As(err, &refused) {
		t.Errorf("a %d was reported as a refusal", http.StatusTooManyRequests)
	}
	if !errors.Is(err, failure) {
		t.Errorf("got %v, want it to wrap Azure's error", err)
	}
}

// TestDiscoverEndpoints_SingleGroupWithMultihop covers the shape Azure emits:
// one group for the whole vnet, no node selector, and multihop on every
// neighbour because the Route Server is off the node's link.
func TestDiscoverEndpoints_SingleGroupWithMultihop(t *testing.T) {
	p := &Platform{
		cfg:  Config{RouteServerName: "rs"},
		topo: &fakeTopo{t: &RouteServerTopology{ASN: 65515, Addresses: []string{"10.0.1.4", "10.0.1.5"}}},
	}

	result, err := p.DiscoverEndpoints(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.PeerGroups) != 1 {
		t.Fatalf("got %d peer groups, want 1", len(result.PeerGroups))
	}
	g := result.PeerGroups[0]
	if g.Key != "rs" {
		t.Errorf("key = %q, want the Route Server name", g.Key)
	}
	if len(g.NodeSelector) != 0 {
		t.Errorf("nodeSelector = %v, want empty so every router node is covered", g.NodeSelector)
	}
	if len(g.Neighbors) != 2 {
		t.Fatalf("got %d neighbours, want 2", len(g.Neighbors))
	}
	for _, n := range g.Neighbors {
		if n.ASN != 65515 {
			t.Errorf("neighbour %s ASN = %d, want 65515", n.Address, n.ASN)
		}
		if !n.EBGPMultiHop {
			t.Errorf("neighbour %s does not ask for multihop", n.Address)
		}
	}
}

// TestDiscoverEndpoints_Refusals covers a Route Server that cannot be peered
// with, rather than emitting a group with nothing in it.
func TestDiscoverEndpoints_Refusals(t *testing.T) {
	for name, topo := range map[string]*RouteServerTopology{
		"no addresses": {ASN: 65515},
		"no ASN":       {Addresses: []string{"10.0.1.4"}},
	} {
		p := &Platform{cfg: Config{RouteServerName: "rs"}, topo: &fakeTopo{t: topo}}
		if _, err := p.DiscoverEndpoints(context.Background()); err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
	}
}

// TestToVirtualMachines_WrapsBadProviderIDWithNodeName pins that a node that
// cannot be resolved to a VM fails with its own name in the error, since
// "not an Azure VM provider ID" alone does not say which of several router
// nodes is the problem.
func TestToVirtualMachines_WrapsBadProviderIDWithNodeName(t *testing.T) {
	nodes := []platform.RouterNode{
		{Name: "good-node", ProviderID: testProviderID},
		{Name: "bad-node", ProviderID: "not-a-provider-id"},
	}
	_, err := toVirtualMachines(nodes)
	if err == nil {
		t.Fatal("expected an error for the unparseable node")
	}
	if !strings.Contains(err.Error(), "bad-node") {
		t.Errorf("error %q does not name the offending node", err.Error())
	}
}

// TestReconcileNodes_ForwardingFailureSkipsPeering pins the ordering the code
// comments call out: a node whose interface cannot be found for forwarding
// must not reach peering reconciliation, since a peering to a node dropping
// forwarded packets is the failure that looks healthy.
func TestReconcileNodes_ForwardingFailureSkipsPeering(t *testing.T) {
	rs := &fakeRS{}
	p := &Platform{
		cfg:  Config{ClusterID: "cluster", LocalASN: 65001},
		rs:   rs,
		nics: &fakeNICs{}, // no NICs, so forwarding cannot find one
	}

	nodes := []platform.RouterNode{{Name: "n1", PrivateIP: "10.0.128.4", ProviderID: testProviderID}}
	if err := p.ReconcileNodes(context.Background(), nodes); err == nil {
		t.Fatal("expected the forwarding failure to be reported")
	}
	if rs.desired != nil {
		t.Errorf("peering reconciliation ran despite the forwarding failure: %v", rs.desired)
	}
}

// TestReconcileNodes_EnablesForwardingThenReconcilesPeerings covers the
// success path end to end: the node's interface is turned on, and the
// peering built from it reaches the Route Server.
func TestReconcileNodes_EnablesForwardingThenReconcilesPeerings(t *testing.T) {
	vm, err := ParseProviderID(testProviderID)
	if err != nil {
		t.Fatal(err)
	}
	rs := &fakeRS{}
	nics := &fakeNICs{nics: []NIC{{Name: "nic-1", ResourceGroup: vm.ResourceGroup, VMID: vm.ID}}}
	p := &Platform{
		cfg:  Config{ClusterID: "cluster", LocalASN: 65001, RouteServerName: "rs"},
		rs:   rs,
		nics: nics,
	}

	nodes := []platform.RouterNode{{Name: "n1", PrivateIP: "10.0.128.4", ProviderID: testProviderID}}
	if err := p.ReconcileNodes(context.Background(), nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(nics.enabled) != 1 || nics.enabled[0] != "nic-1" {
		t.Errorf("enabled = %v, want [nic-1]", nics.enabled)
	}
	if len(rs.desired) != 1 || rs.desired[0].PeerIP != "10.0.128.4" || rs.desired[0].PeerASN != 65001 {
		t.Errorf("desired peers = %+v", rs.desired)
	}
}

// TestReconcileNodes_EmptyNodeListLeavesPeeringsAlone pins that an empty
// node list leaves existing peerings alone instead of deleting them all.
func TestReconcileNodes_EmptyNodeListLeavesPeeringsAlone(t *testing.T) {
	rs := &fakeRS{current: []ObservedPeer{
		{Peer: Peer{Name: peeringName("cluster", "10.0.128.4"), PeerIP: "10.0.128.4", PeerASN: 65001}},
	}}
	nics := &fakeNICs{}
	p := &Platform{cfg: Config{ClusterID: "cluster", LocalASN: 65001}, rs: rs, nics: nics}

	if err := p.ReconcileNodes(context.Background(), nil); err != nil {
		t.Fatalf("ReconcileNodes on an empty node list: %v", err)
	}
	if rs.desired != nil {
		t.Errorf("peering reconciliation ran on an empty node list: %v", rs.desired)
	}
	if len(nics.enabled) != 0 {
		t.Errorf("forwarding was touched on an empty node list: %v", nics.enabled)
	}
}

// TestReconcileNodes_ShrunkNodeListStillReconciles guards the early return
// from swallowing the case it must not: a node list that has shrunk but is
// not empty must still reach the Route Server, or a removed node keeps its
// peering forever.
func TestReconcileNodes_ShrunkNodeListStillReconciles(t *testing.T) {
	vm, err := ParseProviderID(testProviderID)
	if err != nil {
		t.Fatal(err)
	}
	rs := &fakeRS{current: []ObservedPeer{
		{Peer: Peer{Name: peeringName("cluster", "10.0.128.4"), PeerIP: "10.0.128.4", PeerASN: 65001}},
		{Peer: Peer{Name: peeringName("cluster", "10.0.128.5"), PeerIP: "10.0.128.5", PeerASN: 65001}},
	}}
	nics := &fakeNICs{nics: []NIC{{Name: "nic-1", ResourceGroup: vm.ResourceGroup, VMID: vm.ID}}}
	p := &Platform{cfg: Config{ClusterID: "cluster", LocalASN: 65001}, rs: rs, nics: nics}

	nodes := []platform.RouterNode{{Name: "n1", PrivateIP: "10.0.128.4", ProviderID: testProviderID}}
	if err := p.ReconcileNodes(context.Background(), nodes); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rs.desired) != 1 || rs.desired[0].PeerIP != "10.0.128.4" {
		t.Errorf("desired peerings = %+v, want only the remaining node's", rs.desired)
	}
}

var _ platform.CloudPlatform = (*Platform)(nil)
