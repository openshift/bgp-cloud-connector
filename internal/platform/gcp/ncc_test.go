package gcp

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/networkconnectivity/v1"
)

func spokeWith(siteToSite bool, instances ...*networkconnectivity.RouterApplianceInstance) *networkconnectivity.Spoke {
	return &networkconnectivity.Spoke{
		LinkedRouterApplianceInstances: &networkconnectivity.LinkedRouterApplianceInstances{
			SiteToSiteDataTransfer: siteToSite,
			Instances:              instances,
		},
	}
}

func appliance(name, ip string) *networkconnectivity.RouterApplianceInstance {
	return &networkconnectivity.RouterApplianceInstance{
		VirtualMachine: SelfLink("proj", "us-east1-b", name),
		IpAddress:      ip,
	}
}

func routerNode(name, ip string) RouterNode {
	return RouterNode{
		Name:      name,
		SelfLink:  SelfLink("proj", "us-east1-b", name),
		Zone:      "us-east1-b",
		IPAddress: ip,
	}
}

// TestSpokeMatches covers the three things a spoke carries. Comparing only the
// instance self links leaves the other two unchecked, so a node keeping its
// instance while changing address, or siteToSiteDataTransfer being flipped in
// the spec, reads as nothing to do and no patch is ever sent.
func TestSpokeMatches(t *testing.T) {
	for _, tc := range []struct {
		name       string
		spoke      *networkconnectivity.Spoke
		nodes      []RouterNode
		siteToSite bool
		want       bool
	}{
		{
			name:  "identical",
			spoke: spokeWith(false, appliance("worker-a", "10.0.1.4")),
			nodes: []RouterNode{routerNode("worker-a", "10.0.1.4")},
			want:  true,
		},
		{
			name:  "a node joins",
			spoke: spokeWith(false, appliance("worker-a", "10.0.1.4")),
			nodes: []RouterNode{routerNode("worker-a", "10.0.1.4"), routerNode("worker-b", "10.0.1.5")},
			want:  false,
		},
		{
			name:  "a node leaves",
			spoke: spokeWith(false, appliance("worker-a", "10.0.1.4"), appliance("worker-b", "10.0.1.5")),
			nodes: []RouterNode{routerNode("worker-a", "10.0.1.4")},
			want:  false,
		},
		{
			name:  "a node keeps its instance and changes address",
			spoke: spokeWith(false, appliance("worker-a", "10.0.1.4")),
			nodes: []RouterNode{routerNode("worker-a", "10.0.1.9")},
			want:  false,
		},
		{
			name:       "site-to-site is turned on in the spec",
			spoke:      spokeWith(false, appliance("worker-a", "10.0.1.4")),
			nodes:      []RouterNode{routerNode("worker-a", "10.0.1.4")},
			siteToSite: true,
			want:       false,
		},
		{
			name:       "site-to-site is turned off in the spec",
			spoke:      spokeWith(true, appliance("worker-a", "10.0.1.4")),
			nodes:      []RouterNode{routerNode("worker-a", "10.0.1.4")},
			siteToSite: false,
			want:       false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := spokeMatches(tc.spoke, tc.nodes, tc.siteToSite); got != tc.want {
				t.Errorf("spokeMatches = %v, want %v", got, tc.want)
			}
		})
	}
}

// fakeNCCAPI substitutes the live Network Connectivity Center service at the
// nccAPI seam so the reconcile logic on nccClient can be driven without reaching
// Google. It records every write and can serve List results across pages.
type fakeNCCAPI struct {
	spoke  *networkconnectivity.Spoke
	getErr error

	pages [][]*networkconnectivity.Spoke // one entry per List page

	deleteErr error

	createCalls []createSpokeCall
	patchCalls  []patchSpokeCall
	deleted     []string
}

type createSpokeCall struct {
	parent, spokeID string
	spoke           *networkconnectivity.Spoke
}

type patchSpokeCall struct {
	name  string
	spoke *networkconnectivity.Spoke
	mask  string
}

func (f *fakeNCCAPI) GetSpoke(context.Context, string) (*networkconnectivity.Spoke, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.spoke, nil
}

func (f *fakeNCCAPI) CreateSpoke(_ context.Context, parent, spokeID string, spoke *networkconnectivity.Spoke) (*networkconnectivity.GoogleLongrunningOperation, error) {
	f.createCalls = append(f.createCalls, createSpokeCall{parent: parent, spokeID: spokeID, spoke: spoke})
	return &networkconnectivity.GoogleLongrunningOperation{Name: "op"}, nil
}

func (f *fakeNCCAPI) PatchSpoke(_ context.Context, name string, spoke *networkconnectivity.Spoke, mask string) (*networkconnectivity.GoogleLongrunningOperation, error) {
	f.patchCalls = append(f.patchCalls, patchSpokeCall{name: name, spoke: spoke, mask: mask})
	return &networkconnectivity.GoogleLongrunningOperation{Name: "op"}, nil
}

func (f *fakeNCCAPI) DeleteSpoke(_ context.Context, name string) (*networkconnectivity.GoogleLongrunningOperation, error) {
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.deleted = append(f.deleted, name)
	return &networkconnectivity.GoogleLongrunningOperation{Name: "op"}, nil
}

func (f *fakeNCCAPI) ListSpokes(_ context.Context, _, pageToken string) (*networkconnectivity.ListSpokesResponse, error) {
	idx := 0
	if pageToken != "" {
		idx, _ = strconv.Atoi(pageToken)
	}
	resp := &networkconnectivity.ListSpokesResponse{Spokes: f.pages[idx]}
	if idx+1 < len(f.pages) {
		resp.NextPageToken = strconv.Itoa(idx + 1)
	}
	return resp, nil
}

func (f *fakeNCCAPI) WaitLRO(context.Context, *networkconnectivity.GoogleLongrunningOperation) error {
	return nil
}

func testNCCClient(api nccAPI) *nccClient {
	return &nccClient{api: api, project: "proj", region: "us-east1"}
}

// TestReconcileSpoke_CreatesWhenAbsent covers the 404 branch: a spoke that does
// not exist yet is created, carrying the hub, the site-to-site flag and one
// appliance instance per node.
func TestReconcileSpoke_CreatesWhenAbsent(t *testing.T) {
	api := &fakeNCCAPI{getErr: &googleapi.Error{Code: 404}}
	c := testNCCClient(api)

	nodes := []RouterNode{routerNode("worker-a", "10.0.1.4")}
	changed, err := c.ReconcileSpoke(context.Background(), "cluster-bgp-spoke-0", "cluster-hub", nodes, true)
	if err != nil {
		t.Fatalf("ReconcileSpoke: %v", err)
	}
	if !changed {
		t.Fatalf("reported no change when a spoke had to be created")
	}
	if len(api.createCalls) != 1 {
		t.Fatalf("sent %d creates, want 1", len(api.createCalls))
	}
	call := api.createCalls[0]
	if call.spokeID != "cluster-bgp-spoke-0" {
		t.Errorf("spoke id = %q, want cluster-bgp-spoke-0", call.spokeID)
	}
	if want := hubPath("proj", "cluster-hub"); call.spoke.Hub != want {
		t.Errorf("hub = %q, want %q", call.spoke.Hub, want)
	}
	linked := call.spoke.LinkedRouterApplianceInstances
	if linked == nil || !linked.SiteToSiteDataTransfer {
		t.Fatalf("site-to-site data transfer was not carried into the create")
	}
	if len(linked.Instances) != 1 || linked.Instances[0].VirtualMachine != routerNode("worker-a", "10.0.1.4").SelfLink {
		t.Errorf("appliance instances = %v, want one for worker-a", linked.Instances)
	}
}

// TestReconcileSpoke_NoPatchWhenInstancesReordered guards idempotency against
// node ordering: the controller may report nodes in any order, and spokeMatches
// compares instances as a set, so a spoke that already lists the same instances
// in a different order must send no patch. Comparing ordered slices instead
// would patch on every reconcile and churn the spoke forever.
func TestReconcileSpoke_NoPatchWhenInstancesReordered(t *testing.T) {
	api := &fakeNCCAPI{spoke: spokeWith(true,
		appliance("worker-b", "10.0.1.5"),
		appliance("worker-a", "10.0.1.4"),
	)}
	c := testNCCClient(api)

	nodes := []RouterNode{
		routerNode("worker-a", "10.0.1.4"),
		routerNode("worker-b", "10.0.1.5"),
	}
	changed, err := c.ReconcileSpoke(context.Background(), "cluster-bgp-spoke-0", "cluster-hub", nodes, true)
	if err != nil {
		t.Fatalf("ReconcileSpoke: %v", err)
	}
	if changed {
		t.Errorf("reported a change for a spoke whose instances differ only in order")
	}
	if len(api.patchCalls) != 0 {
		t.Errorf("patched a spoke that already matched but for instance order")
	}
}

// TestReconcileSpoke_PatchesWhenOnlySiteToSiteFlips guards a documented past
// bug: the instances are unchanged and only spec.gcp.ncc.siteToSiteDataTransfer
// is turned on. Comparing the instances alone read this as nothing to do, so the
// flag was never applied. The patch has to be sent and has to carry the flag.
func TestReconcileSpoke_PatchesWhenOnlySiteToSiteFlips(t *testing.T) {
	api := &fakeNCCAPI{spoke: spokeWith(false, appliance("worker-a", "10.0.1.4"))}
	c := testNCCClient(api)

	nodes := []RouterNode{routerNode("worker-a", "10.0.1.4")}
	changed, err := c.ReconcileSpoke(context.Background(), "cluster-bgp-spoke-0", "cluster-hub", nodes, true)
	if err != nil {
		t.Fatalf("ReconcileSpoke: %v", err)
	}
	if !changed {
		t.Fatalf("sent no patch when site-to-site was flipped on with instances unchanged")
	}
	if len(api.patchCalls) != 1 {
		t.Fatalf("sent %d patches, want 1", len(api.patchCalls))
	}
	if !api.patchCalls[0].spoke.LinkedRouterApplianceInstances.SiteToSiteDataTransfer {
		t.Errorf("patch did not carry site-to-site data transfer on")
	}
}

// TestReconcileSpoke_PatchesOnDrift covers a node keeping its instance while
// changing address: the spoke has to be patched, with the update mask that names
// both fields the reconcile can change.
func TestReconcileSpoke_PatchesOnDrift(t *testing.T) {
	api := &fakeNCCAPI{spoke: spokeWith(true, appliance("worker-a", "10.0.1.4"))}
	c := testNCCClient(api)

	nodes := []RouterNode{routerNode("worker-a", "10.0.1.9")}
	changed, err := c.ReconcileSpoke(context.Background(), "cluster-bgp-spoke-0", "cluster-hub", nodes, true)
	if err != nil {
		t.Fatalf("ReconcileSpoke: %v", err)
	}
	if !changed {
		t.Fatalf("reported no change when the spoke had drifted")
	}
	if len(api.patchCalls) != 1 {
		t.Fatalf("sent %d patches, want 1", len(api.patchCalls))
	}
	const wantMask = "linkedRouterApplianceInstances.instances,linkedRouterApplianceInstances.siteToSiteDataTransfer"
	if api.patchCalls[0].mask != wantMask {
		t.Errorf("update mask = %q, want %q", api.patchCalls[0].mask, wantMask)
	}
	insts := api.patchCalls[0].spoke.LinkedRouterApplianceInstances.Instances
	if len(insts) != 1 || insts[0].IpAddress != "10.0.1.9" {
		t.Errorf("patched instances = %v, want worker-a at 10.0.1.9", insts)
	}
}

// TestDeleteSpoke_ToleratesAlreadyGone covers Cleanup racing another delete: a
// 404 means the spoke is already gone, which is the desired end state, not a
// failure. Without this, a racing delete would fail cleanup and strand the
// finalizer.
func TestDeleteSpoke_ToleratesAlreadyGone(t *testing.T) {
	api := &fakeNCCAPI{deleteErr: &googleapi.Error{Code: 404}}
	c := testNCCClient(api)

	deleted, err := c.DeleteSpoke(context.Background(), "cluster-bgp-spoke-0")
	if err != nil {
		t.Errorf("a spoke that is already gone must not fail cleanup: %v", err)
	}
	if deleted {
		t.Errorf("reported a delete for a spoke that was already gone")
	}
}

// TestNCCOpDone covers the long-running-operation terminal-state check WaitLRO
// stops its poll loop on: an operation not yet done keeps it polling, a done
// operation stops it, and a done operation carrying an error surfaces that error
// with its code and message rather than reporting success. This is the one bit
// of logic in that loop and it sits behind an unmockable time.After, so it is
// tested here in isolation.
func TestNCCOpDone(t *testing.T) {
	if done, err := nccOpDone(&networkconnectivity.GoogleLongrunningOperation{Done: false}); done || err != nil {
		t.Errorf("nccOpDone(not done) = (%v, %v), want (false, nil)", done, err)
	}
	if done, err := nccOpDone(&networkconnectivity.GoogleLongrunningOperation{Done: true}); !done || err != nil {
		t.Errorf("nccOpDone(done) = (%v, %v), want (true, nil)", done, err)
	}
	done, err := nccOpDone(&networkconnectivity.GoogleLongrunningOperation{
		Done:  true,
		Error: &networkconnectivity.GoogleRpcStatus{Code: 7, Message: "permission denied"},
	})
	if !done || err == nil {
		t.Fatalf("nccOpDone(done with error) = (%v, %v), want (true, non-nil)", done, err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error %q does not carry the operation's message", err)
	}
}

// TestListSpokesByPrefix_PaginatesAndFilters is the pagination edge case: the
// wanted spokes are split across two pages, and the list also carries spokes
// this call must ignore — one on another hub, one whose suffix is not a number,
// and one whose id does not carry the prefix.
func TestListSpokesByPrefix_PaginatesAndFilters(t *testing.T) {
	hub := hubPath("proj", "cluster-hub")
	other := hubPath("proj", "other-hub")
	sp := func(id, h string) *networkconnectivity.Spoke {
		return &networkconnectivity.Spoke{
			Name: "projects/proj/locations/us-east1/spokes/" + id,
			Hub:  h,
		}
	}
	api := &fakeNCCAPI{pages: [][]*networkconnectivity.Spoke{
		{
			sp("cluster-bgp-spoke-0", hub),
			sp("cluster-bgp-spoke-x", hub), // non-numeric suffix, not ours
		},
		{
			sp("cluster-bgp-spoke-1", hub),
			sp("cluster-bgp-spoke-2", other), // another hub
			sp("unrelated-9", hub),           // wrong prefix
		},
	}}
	c := testNCCClient(api)

	ids, err := c.ListSpokesByPrefix(context.Background(), "cluster-hub", "cluster-bgp-spoke")
	if err != nil {
		t.Fatalf("ListSpokesByPrefix: %v", err)
	}
	want := []string{"cluster-bgp-spoke-0", "cluster-bgp-spoke-1"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Errorf("id %d = %q, want %q", i, ids[i], id)
		}
	}
}
