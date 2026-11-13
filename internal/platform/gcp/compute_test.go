package gcp

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/api/compute/v1"
)

func nodes(ips ...string) []RouterNode {
	out := make([]RouterNode, 0, len(ips))
	for i, ip := range ips {
		out = append(out, RouterNode{
			Name:      string(rune('a'+i)) + "-worker",
			IPAddress: ip,
			SelfLink:  SelfLink("proj", "us-east1-b", string(rune('a'+i))+"-worker"),
		})
	}
	return out
}

func topology() *CloudRouterTopology {
	return &CloudRouterTopology{
		CloudRouterASN: 65000,
		InterfaceNames: []string{"if0", "if1"},
		InterfaceIPs:   []string{"10.0.0.2", "10.0.0.3"},
	}
}

func namesByPeerIP(peers []*compute.RouterBgpPeer) map[string][]string {
	out := map[string][]string{}
	for _, p := range peers {
		out[p.PeerIpAddress] = append(out[p.PeerIpAddress], p.Name)
	}
	return out
}

// TestDesiredPeers_NamesSurviveNodeRemoval pins that a node leaving does not
// rename the peers of the nodes that stay.
//
// A Cloud Router patch replaces the whole peer list, so a rename is a delete
// and a create: a name that moved with the node set would tear down and
// re-establish the BGP session of every surviving node.
func TestDesiredPeers_NamesSurviveNodeRemoval(t *testing.T) {
	before := namesByPeerIP(desiredPeers("cluster", nodes("10.0.1.4", "10.0.1.5", "10.0.1.6"), topology(), 65001))
	after := namesByPeerIP(desiredPeers("cluster", nodes("10.0.1.5", "10.0.1.6"), topology(), 65001))

	for _, ip := range []string{"10.0.1.5", "10.0.1.6"} {
		if len(after[ip]) != len(before[ip]) {
			t.Fatalf("%s: got %d peers, want %d", ip, len(after[ip]), len(before[ip]))
		}
		for i := range before[ip] {
			if before[ip][i] != after[ip][i] {
				t.Errorf("%s peer %d renamed by an unrelated node leaving: %q -> %q",
					ip, i, before[ip][i], after[ip][i])
			}
		}
	}
}

// TestMergePeers_LeavesOtherClustersAlone covers a Cloud Router shared between
// clusters. Routers.Patch uses JSON merge patch, so the peer array is replaced
// wholesale and anything left out of the list is deleted.
func TestMergePeers_LeavesOtherClustersAlone(t *testing.T) {
	foreign := []*compute.RouterBgpPeer{
		{Name: "other-bgp-10-9-9-9-0", PeerIpAddress: "10.9.9.9"},
		{Name: "hand-made-peer", PeerIpAddress: "10.9.9.10"},
	}
	ours := desiredPeers("cluster", nodes("10.0.1.4"), topology(), 65001)

	merged := mergePeers(append(append([]*compute.RouterBgpPeer{}, foreign...), ours...), ours, "cluster")

	for _, want := range foreign {
		found := false
		for _, p := range merged {
			if p.Name == want.Name {
				found = true
			}
		}
		if !found {
			t.Errorf("peer %q belongs to another cluster and was dropped", want.Name)
		}
	}
	if len(merged) != len(foreign)+len(ours) {
		t.Errorf("merged %d peers, want %d", len(merged), len(foreign)+len(ours))
	}
}

// TestIsOurPeer_DoesNotClaimALongerClusterName pins the separator: without it
// "cluster" would own the peers of "cluster-two".
func TestIsOurPeer_DoesNotClaimALongerClusterName(t *testing.T) {
	name := PeerName("cluster-two", "10.0.1.4", 0)
	if isOurPeer(name, "cluster") {
		t.Errorf("%q was claimed by cluster %q", name, "cluster")
	}
	if !isOurPeer(name, "cluster-two") {
		t.Errorf("%q was not claimed by its own cluster", name)
	}
}

// TestPeerName_WithinGCELimit covers a long infrastructure name, which is
// where the 63 character limit bites.
//
// Fitting the limit is only half of it. Whatever the name gives up to fit, it
// has to stay recognisable to isOurPeer, or the cluster stops recognising its
// own peers: mergePeers holds on to them as another cluster's and Cleanup
// leaves them on the router.
func TestPeerName_WithinGCELimit(t *testing.T) {
	const cluster = "a-very-long-openshift-infrastructure-name-with-suffix-abcde"

	name := PeerName(cluster, "10.128.128.128", 1)
	if len(name) > maxPeerNameLength {
		t.Errorf("peer name is %d characters, over the %d limit: %q", len(name), maxPeerNameLength, name)
	}
	if !isOurPeer(name, cluster) {
		t.Errorf("%q is not recognised as belonging to the cluster that built it", name)
	}
}

// TestPeerName_AbbreviatedPrefixesDoNotCollide covers two clusters whose names
// are too long to carry whole and share everything but their ending.
//
// Abbreviating by cutting would give both the same prefix, and the prefix is
// the only ownership marker a Cloud Router peer can carry, so each cluster
// would treat the other's peers as its own and delete them.
func TestPeerName_AbbreviatedPrefixesDoNotCollide(t *testing.T) {
	shared := strings.Repeat("x", 50)
	alpha, beta := shared+"-alpha", shared+"-beta"

	name := PeerName(alpha, "10.0.1.4", 0)
	if !isOurPeer(name, alpha) {
		t.Errorf("%q is not recognised by the cluster that built it", name)
	}
	if isOurPeer(name, beta) {
		t.Errorf("%q built for %q was claimed by %q", name, alpha, beta)
	}
}

// TestBuildPeerSet_SeesInterfaceDrift covers a Cloud Router interface being
// rebuilt under a peer that keeps its name.
//
// The name is keyed on the node address and the interface index, so renaming
// or re-addressing an interface leaves every name unchanged. If the comparison
// key does not carry what the interface contributes, the desired set equals the
// current one, no patch is sent, and the peer keeps pointing at an interface
// that is gone.
func TestBuildPeerSet_SeesInterfaceDrift(t *testing.T) {
	n := nodes("10.0.1.4")

	before := desiredPeers("cluster", n, topology(), 65001)
	rebuilt := &CloudRouterTopology{
		CloudRouterASN: 65000,
		InterfaceNames: []string{"if0-rebuilt", "if1-rebuilt"},
		InterfaceIPs:   []string{"10.0.0.8", "10.0.0.9"},
	}
	after := desiredPeers("cluster", n, rebuilt, 65001)

	if before[0].Name != after[0].Name {
		t.Fatalf("peer names differ (%q vs %q); this test is meaningless unless they match",
			before[0].Name, after[0].Name)
	}
	if buildPeerSet(before).Equal(buildPeerSet(after)) {
		t.Errorf("peer sets compare equal across an interface rebuild, so no patch would be sent:\n"+
			" before: interface %q at %q\n after:  interface %q at %q",
			before[0].InterfaceName, before[0].IpAddress, after[0].InterfaceName, after[0].IpAddress)
	}
}

// TestBuildPeerSet_SeesApplianceDrift covers a node keeping its address while
// its instance is replaced, which reissues the self link the peer names.
func TestBuildPeerSet_SeesApplianceDrift(t *testing.T) {
	before := desiredPeers("cluster", nodes("10.0.1.4"), topology(), 65001)

	replaced := []RouterNode{{
		Name:      "a-worker",
		IPAddress: "10.0.1.4",
		SelfLink:  SelfLink("proj", "us-east1-b", "a-worker-rebuilt"),
	}}
	after := desiredPeers("cluster", replaced, topology(), 65001)

	if buildPeerSet(before).Equal(buildPeerSet(after)) {
		t.Errorf("peer sets compare equal across an instance replacement, so no patch would be sent:\n"+
			" before: %q\n after:  %q", before[0].RouterApplianceInstance, after[0].RouterApplianceInstance)
	}
}

// TestPeerName_IsAValidGCEName covers a dual-stack node, whose first internal
// address the controller reports may be IPv6.
//
// A GCE resource name accepts lowercase letters, digits and dashes, and must
// start with a letter. An address carries separators that are none of those,
// so a name built by substituting only the IPv4 dot is rejected by the API and
// no peer is created at all.
func TestPeerName_IsAValidGCEName(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

	for _, address := range []string{
		"10.0.128.2",
		"2a00:8a00:4000:780::4",
		"fd01:0:0:1::4",
		"2001:DB8:85A3:8D3:1319:8A2E:370:7348",
	} {
		name := PeerName("cluster-abcde", address, 0)
		if !valid.MatchString(name) {
			t.Errorf("peer name for %q is not a valid GCE name: %q", address, name)
		}
		if len(name) > maxPeerNameLength {
			t.Errorf("peer name for %q is %d characters, over the %d limit: %q",
				address, len(name), maxPeerNameLength, name)
		}
	}
}

// TestPeerName_DistinguishesAddresses guards the substitution from mapping two
// different node addresses onto one name, which would leave the second node
// without a peer of its own.
func TestPeerName_DistinguishesAddresses(t *testing.T) {
	seen := map[string]string{}
	for _, address := range []string{
		"10.0.128.2", "10.0.128.3",
		"fd01:0:0:1::4", "fd01:0:0:1::5", "fd01:0:0:2::4",
	} {
		name := PeerName("cluster-abcde", address, 0)
		if other, dup := seen[name]; dup {
			t.Errorf("%q and %q both produce peer name %q", other, address, name)
		}
		seen[name] = address
	}
}

// fakeComputeAPI substitutes the live GCE service at the computeAPI seam so the
// reconcile logic on computeClient can be driven without reaching Google. It
// records every write so a test can assert what would have been sent.
type fakeComputeAPI struct {
	instances map[string]*compute.Instance // keyed by "zone/name"
	router    *compute.Router

	updateInstanceCalls []updateInstanceCall
	patchCalls          []*compute.Router
	updateCalls         []*compute.Router
}

type updateInstanceCall struct {
	zone, name           string
	inst                 *compute.Instance
	mostDisruptiveAction string
}

func (f *fakeComputeAPI) GetInstance(_ context.Context, zone, name string) (*compute.Instance, error) {
	inst, ok := f.instances[zone+"/"+name]
	if !ok {
		return nil, fmt.Errorf("instance %s/%s not found", zone, name)
	}
	return inst, nil
}

func (f *fakeComputeAPI) UpdateInstance(_ context.Context, zone, name string, inst *compute.Instance, action string) (*compute.Operation, error) {
	f.updateInstanceCalls = append(f.updateInstanceCalls, updateInstanceCall{
		zone: zone, name: name, inst: inst, mostDisruptiveAction: action,
	})
	return &compute.Operation{Name: "op"}, nil
}

func (f *fakeComputeAPI) GetRouter(context.Context, string) (*compute.Router, error) {
	return f.router, nil
}

func (f *fakeComputeAPI) PatchRouter(_ context.Context, _ string, patch *compute.Router) (*compute.Operation, error) {
	f.patchCalls = append(f.patchCalls, patch)
	return &compute.Operation{Name: "op"}, nil
}

func (f *fakeComputeAPI) UpdateRouter(_ context.Context, _ string, r *compute.Router) (*compute.Operation, error) {
	f.updateCalls = append(f.updateCalls, r)
	return &compute.Operation{Name: "op"}, nil
}

func (f *fakeComputeAPI) WaitZoneOp(context.Context, string, *compute.Operation) error { return nil }
func (f *fakeComputeAPI) WaitRegionOp(context.Context, *compute.Operation) error       { return nil }

// TestReconcilePeers_NoOpWhenRouterAlreadyMatches is the idempotency guard: a
// second reconcile of an unchanged node set must send no patch. A Cloud Router
// patch replaces the whole peer list, so a needless write churns every session.
func TestReconcilePeers_NoOpWhenRouterAlreadyMatches(t *testing.T) {
	top := topology()
	n := nodes("10.0.1.4")
	api := &fakeComputeAPI{router: &compute.Router{BgpPeers: desiredPeers("cluster", n, top, 65001)}}
	c := &computeClient{api: api}

	changed, err := c.ReconcilePeers(context.Background(), "cr", "cluster", n, top, 65001)
	if err != nil {
		t.Fatalf("ReconcilePeers: %v", err)
	}
	if changed {
		t.Errorf("reported a change when the router already carried the desired peers")
	}
	if len(api.patchCalls) != 0 {
		t.Errorf("sent %d patches for an unchanged router, want 0", len(api.patchCalls))
	}
}

// TestReconcilePeers_PatchesWhenInterfaceDriftsUnderStablePeerNames is the
// subtle correctness property of the whole method: peer names are keyed on the
// node address and interface index, so a Cloud Router interface re-addressed
// under the same index leaves every name unchanged. If ReconcilePeers compared
// names alone it would send no patch and leave every peer pointing at an
// interface IP that no longer exists — the session would never come back up.
func TestReconcilePeers_PatchesWhenInterfaceDriftsUnderStablePeerNames(t *testing.T) {
	n := nodes("10.0.1.4")
	oldTop := topology()
	existing := desiredPeers("cluster", n, oldTop, 65001)

	newTop := &CloudRouterTopology{
		CloudRouterASN: oldTop.CloudRouterASN,
		InterfaceNames: oldTop.InterfaceNames,
		InterfaceIPs:   []string{"10.0.0.8", "10.0.0.9"}, // interfaces re-addressed
	}
	// The test only means something if the names really are unchanged.
	want := desiredPeers("cluster", n, newTop, 65001)
	for i := range existing {
		if existing[i].Name != want[i].Name {
			t.Fatalf("peer names differ (%q vs %q); this test is meaningless unless they match",
				existing[i].Name, want[i].Name)
		}
	}

	api := &fakeComputeAPI{router: &compute.Router{BgpPeers: existing}}
	c := &computeClient{api: api}

	changed, err := c.ReconcilePeers(context.Background(), "cr", "cluster", n, newTop, 65001)
	if err != nil {
		t.Fatalf("ReconcilePeers: %v", err)
	}
	if !changed {
		t.Fatalf("sent no patch when a Cloud Router interface was re-addressed under a stable peer name")
	}
	if len(api.patchCalls) != 1 {
		t.Fatalf("sent %d patches, want 1", len(api.patchCalls))
	}
	for _, p := range api.patchCalls[0].BgpPeers {
		if isOurPeer(p.Name, "cluster") && p.IpAddress != "10.0.0.8" && p.IpAddress != "10.0.0.9" {
			t.Errorf("patched peer %q still points at the old interface IP %q", p.Name, p.IpAddress)
		}
	}
}

// TestClearPeers_RemovesOnlyOurs covers Cleanup on a shared router: our peers
// go, everyone else's stay.
func TestClearPeers_RemovesOnlyOurs(t *testing.T) {
	top := topology()
	foreign := &compute.RouterBgpPeer{Name: "other-bgp-10-9-9-9-0", PeerIpAddress: "10.9.9.9"}
	all := append([]*compute.RouterBgpPeer{foreign}, desiredPeers("cluster", nodes("10.0.1.4"), top, 65001)...)
	api := &fakeComputeAPI{router: &compute.Router{BgpPeers: all}}
	c := &computeClient{api: api}

	changed, err := c.ClearPeers(context.Background(), "cr", "cluster")
	if err != nil {
		t.Fatalf("ClearPeers: %v", err)
	}
	if !changed {
		t.Fatalf("reported no change when our peers had to be removed")
	}
	if len(api.updateCalls) != 1 {
		t.Fatalf("sent %d updates, want 1", len(api.updateCalls))
	}
	kept := api.updateCalls[0].BgpPeers
	if len(kept) != 1 || kept[0].Name != foreign.Name {
		t.Errorf("kept %v, want only the foreign peer %q", kept, foreign.Name)
	}
}

// TestClearPeers_NoOpWhenNoneOurs guards against an empty write, which would be
// a needless router update, when there is nothing of ours to remove.
func TestClearPeers_NoOpWhenNoneOurs(t *testing.T) {
	foreign := &compute.RouterBgpPeer{Name: "other-bgp-10-9-9-9-0", PeerIpAddress: "10.9.9.9"}
	api := &fakeComputeAPI{router: &compute.Router{BgpPeers: []*compute.RouterBgpPeer{foreign}}}
	c := &computeClient{api: api}

	changed, err := c.ClearPeers(context.Background(), "cr", "cluster")
	if err != nil {
		t.Fatalf("ClearPeers: %v", err)
	}
	if changed {
		t.Errorf("reported a change when no owned peers existed")
	}
	if len(api.updateCalls) != 0 {
		t.Errorf("updated the router with nothing of ours to remove")
	}
}

// TestEnsureCanIPForward_EnablesWhenOff pins the field written and the disruption
// level: enabling IP forwarding is a REFRESH, never a restart.
func TestEnsureCanIPForward_EnablesWhenOff(t *testing.T) {
	api := &fakeComputeAPI{instances: map[string]*compute.Instance{
		"us-east1-b/worker-a": {CanIpForward: false},
	}}
	c := &computeClient{api: api}

	changed, err := c.EnsureCanIPForward(context.Background(), routerNode("worker-a", "10.0.1.4"))
	if err != nil {
		t.Fatalf("EnsureCanIPForward: %v", err)
	}
	if !changed {
		t.Fatalf("reported no change when forwarding was off")
	}
	if len(api.updateInstanceCalls) != 1 {
		t.Fatalf("sent %d instance updates, want 1", len(api.updateInstanceCalls))
	}
	call := api.updateInstanceCalls[0]
	if !call.inst.CanIpForward {
		t.Errorf("updated instance still has canIpForward off")
	}
	if call.mostDisruptiveAction != "REFRESH" {
		t.Errorf("disruption level = %q, want REFRESH", call.mostDisruptiveAction)
	}
}

// TestEnsureNestedVirtualization_InitializesAdvancedFeatures covers the nil
// AdvancedMachineFeatures case: the method has to allocate the struct before
// setting the flag, and this field demands a RESTART.
func TestEnsureNestedVirtualization_InitializesAdvancedFeatures(t *testing.T) {
	api := &fakeComputeAPI{instances: map[string]*compute.Instance{
		"us-east1-b/worker-a": {AdvancedMachineFeatures: nil},
	}}
	c := &computeClient{api: api}

	changed, err := c.EnsureNestedVirtualization(context.Background(), routerNode("worker-a", "10.0.1.4"))
	if err != nil {
		t.Fatalf("EnsureNestedVirtualization: %v", err)
	}
	if !changed {
		t.Fatalf("reported no change when nested virtualization was off")
	}
	call := api.updateInstanceCalls[0]
	if call.inst.AdvancedMachineFeatures == nil || !call.inst.AdvancedMachineFeatures.EnableNestedVirtualization {
		t.Errorf("nested virtualization was not enabled on the updated instance")
	}
	if call.mostDisruptiveAction != "RESTART" {
		t.Errorf("disruption level = %q, want RESTART", call.mostDisruptiveAction)
	}
}

// TestEnsureNestedVirtualization_NoOpWhenAlreadyEnabled matters because this
// field's update is a RESTART: a needless write reboots a router node and drops
// its BGP sessions, so an instance already correct must not be touched.
func TestEnsureNestedVirtualization_NoOpWhenAlreadyEnabled(t *testing.T) {
	api := &fakeComputeAPI{instances: map[string]*compute.Instance{
		"us-east1-b/worker-a": {AdvancedMachineFeatures: &compute.AdvancedMachineFeatures{EnableNestedVirtualization: true}},
	}}
	c := &computeClient{api: api}

	changed, err := c.EnsureNestedVirtualization(context.Background(), routerNode("worker-a", "10.0.1.4"))
	if err != nil {
		t.Fatalf("EnsureNestedVirtualization: %v", err)
	}
	if changed {
		t.Errorf("reported a change when nested virtualization was already on")
	}
	if len(api.updateInstanceCalls) != 0 {
		t.Errorf("restarted an instance that already had nested virtualization on")
	}
}

// TestGetRouterTopology_StripsCIDRAndReadsASN pins the two things the topology
// read has to get right: the peer address is the interface IP with any CIDR
// suffix removed, and the Cloud Router ASN comes off the Bgp block.
func TestGetRouterTopology_StripsCIDRAndReadsASN(t *testing.T) {
	api := &fakeComputeAPI{router: &compute.Router{
		Bgp: &compute.RouterBgp{Asn: 65000},
		Interfaces: []*compute.RouterInterface{
			{Name: "if0", IpRange: "10.0.0.2/30"},
			{Name: "if1", IpRange: "10.0.0.6"},
		},
	}}
	c := &computeClient{api: api}

	top, err := c.GetRouterTopology(context.Background(), "cr")
	if err != nil {
		t.Fatalf("GetRouterTopology: %v", err)
	}
	if top.CloudRouterASN != 65000 {
		t.Errorf("ASN = %d, want 65000", top.CloudRouterASN)
	}
	wantIPs := []string{"10.0.0.2", "10.0.0.6"}
	if len(top.InterfaceIPs) != len(wantIPs) {
		t.Fatalf("interface IPs = %v, want %v", top.InterfaceIPs, wantIPs)
	}
	for i, ip := range wantIPs {
		if top.InterfaceIPs[i] != ip {
			t.Errorf("interface IP %d = %q, want %q (CIDR suffix not stripped?)", i, top.InterfaceIPs[i], ip)
		}
	}
}

// TestComputeOpDone covers the operation terminal-state check the WaitZoneOp and
// WaitRegionOp poll loops both stop on: an operation still running keeps them
// polling, a DONE operation stops them, and a DONE operation carrying an error
// surfaces that error rather than reporting success. This is the one bit of
// logic in those loops, and it sits behind an unmockable time.After, so it is
// tested here in isolation.
func TestComputeOpDone(t *testing.T) {
	if done, err := computeOpDone(&compute.Operation{Status: "RUNNING"}); done || err != nil {
		t.Errorf("computeOpDone(RUNNING) = (%v, %v), want (false, nil)", done, err)
	}
	if done, err := computeOpDone(&compute.Operation{Status: "DONE"}); !done || err != nil {
		t.Errorf("computeOpDone(DONE) = (%v, %v), want (true, nil)", done, err)
	}
	done, err := computeOpDone(&compute.Operation{
		Status: "DONE",
		Error:  &compute.OperationError{},
	})
	if !done || err == nil {
		t.Errorf("computeOpDone(DONE with error) = (%v, %v), want (true, non-nil)", done, err)
	}
}

// TestGetRouterTopology_NilBgpYieldsZeroASN covers a Cloud Router with no BGP
// block, which the read has to tolerate rather than dereference.
func TestGetRouterTopology_NilBgpYieldsZeroASN(t *testing.T) {
	api := &fakeComputeAPI{router: &compute.Router{
		Interfaces: []*compute.RouterInterface{{Name: "if0", IpRange: "10.0.0.2/30"}},
	}}
	c := &computeClient{api: api}

	top, err := c.GetRouterTopology(context.Background(), "cr")
	if err != nil {
		t.Fatalf("GetRouterTopology: %v", err)
	}
	if top.CloudRouterASN != 0 {
		t.Errorf("ASN = %d, want 0 for a router with no BGP block", top.CloudRouterASN)
	}
}
