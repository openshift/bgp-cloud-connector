package azure

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
)

// fakeVirtualHubs stands in for armnetwork.VirtualHubsClient.
type fakeVirtualHubs struct {
	hub armnetwork.VirtualHub
}

func (f *fakeVirtualHubs) Get(context.Context, string, string, *armnetwork.VirtualHubsClientGetOptions) (armnetwork.VirtualHubsClientGetResponse, error) {
	return armnetwork.VirtualHubsClientGetResponse{VirtualHub: f.hub}, nil
}

// TestGetTopology_MapsASNAndAddresses pins the field mapping, including that
// an empty-string address (Azure sometimes returns one alongside the real
// pair) is dropped rather than emitted as a neighbour with no address.
func TestGetTopology_MapsASNAndAddresses(t *testing.T) {
	c := &topologyClient{hubs: &fakeVirtualHubs{hub: armnetwork.VirtualHub{
		Properties: &armnetwork.VirtualHubProperties{
			VirtualRouterAsn: to.Ptr(int64(65515)),
			VirtualRouterIPs: []*string{to.Ptr("10.0.1.4"), to.Ptr(""), to.Ptr("10.0.1.5")},
		},
	}}}

	topo, err := c.GetTopology(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.ASN != 65515 {
		t.Errorf("ASN = %d, want 65515", topo.ASN)
	}
	if len(topo.Addresses) != 2 || topo.Addresses[0] != "10.0.1.4" || topo.Addresses[1] != "10.0.1.5" {
		t.Errorf("addresses = %v, want the two non-empty entries", topo.Addresses)
	}
}

// TestGetTopology_NoProperties pins that a hub with no Properties comes back
// as a zero-value topology rather than panicking, since DiscoverEndpoints
// turns an empty topology into a clear refusal rather than a crash.
func TestGetTopology_NoProperties(t *testing.T) {
	c := &topologyClient{hubs: &fakeVirtualHubs{hub: armnetwork.VirtualHub{}}}
	topo, err := c.GetTopology(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if topo.ASN != 0 || len(topo.Addresses) != 0 {
		t.Errorf("got %+v, want a zero-value topology", topo)
	}
}

// fakeInterfaces stands in for armnetwork.InterfacesClient.
type fakeInterfaces struct {
	pages [][]*armnetwork.Interface
	errAt int

	getResp armnetwork.Interface
	updated []armnetwork.Interface
}

func (f *fakeInterfaces) NewListPager(_ string, _ *armnetwork.InterfacesClientListOptions) *runtime.Pager[armnetwork.InterfacesClientListResponse] {
	idx := 0
	return runtime.NewPager(runtime.PagingHandler[armnetwork.InterfacesClientListResponse]{
		More: func(armnetwork.InterfacesClientListResponse) bool {
			return idx < len(f.pages) || idx == f.errAt
		},
		Fetcher: func(context.Context, *armnetwork.InterfacesClientListResponse) (armnetwork.InterfacesClientListResponse, error) {
			if idx == f.errAt {
				return armnetwork.InterfacesClientListResponse{}, errors.New("page boom")
			}
			page := f.pages[idx]
			idx++
			return armnetwork.InterfacesClientListResponse{
				InterfaceListResult: armnetwork.InterfaceListResult{Value: page},
			}, nil
		},
	})
}

func (f *fakeInterfaces) Get(context.Context, string, string, *armnetwork.InterfacesClientGetOptions) (armnetwork.InterfacesClientGetResponse, error) {
	return armnetwork.InterfacesClientGetResponse{Interface: f.getResp}, nil
}

func (f *fakeInterfaces) createOrUpdate(_ context.Context, _, _ string, parameters armnetwork.Interface) error {
	f.updated = append(f.updated, parameters)
	return nil
}

func iface(name, vmID string, forwarding bool) *armnetwork.Interface {
	return &armnetwork.Interface{
		Name: to.Ptr(name),
		Properties: &armnetwork.InterfacePropertiesFormat{
			VirtualMachine:     &armnetwork.SubResource{ID: to.Ptr(vmID)},
			EnableIPForwarding: to.Ptr(forwarding),
		},
	}
}

// TestListNICs_MapsFieldsAndSkipsUnnamed mirrors ListPeers' equivalent: a nil
// entry or one without a name is not something the code can attach a
// resource group to, so it is skipped rather than mapped with a blank name.
func TestListNICs_MapsFieldsAndSkipsUnnamed(t *testing.T) {
	c := &nicClient{interfaces: &fakeInterfaces{errAt: -1, pages: [][]*armnetwork.Interface{{
		nil,
		{Name: nil},
		iface("nic-a", "/subscriptions/x/vm-a", true),
		{Name: to.Ptr("bare")},
	}}}}

	got, err := c.ListNICs(context.Background(), "rg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d NICs, want 2: %+v", len(got), got)
	}
	if got[0].Name != "nic-a" || got[0].VMID != "/subscriptions/x/vm-a" || !got[0].IPForwarding {
		t.Errorf("nic-a = %+v", got[0])
	}
	if got[1].Name != "bare" || got[1].VMID != "" || got[1].IPForwarding {
		t.Errorf("a NIC with no Properties should map to zero values, got %+v", got[1])
	}
	for _, n := range got {
		if n.ResourceGroup != "rg" {
			t.Errorf("resourceGroup = %q, want %q", n.ResourceGroup, "rg")
		}
	}
}

// TestListNICs_Page2Error pins that a failure fetching a later page is
// reported rather than truncating the NIC list silently, since a missing NIC
// looks identical to "no interface is attached to this VM".
func TestListNICs_Page2Error(t *testing.T) {
	c := &nicClient{interfaces: &fakeInterfaces{
		pages: [][]*armnetwork.Interface{{iface("nic-a", "/vm-a", true)}},
		errAt: 1,
	}}
	if _, err := c.ListNICs(context.Background(), "rg"); err == nil {
		t.Fatal("expected the page-2 failure to be reported")
	}
}

// TestEnableIPForwarding_SetsFlagAndWritesBack pins the read-modify-write:
// the flag is set on the object read from Azure, and that whole object,
// unmodified elsewhere, is what gets written back.
func TestEnableIPForwarding_SetsFlagAndWritesBack(t *testing.T) {
	fake := &fakeInterfaces{getResp: armnetwork.Interface{
		Name:       to.Ptr("nic-a"),
		Properties: &armnetwork.InterfacePropertiesFormat{EnableIPForwarding: to.Ptr(false)},
	}}
	c := &nicClient{interfaces: fake}

	if err := c.EnableIPForwarding(context.Background(), "rg", "nic-a"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.updated) != 1 {
		t.Fatalf("got %d update calls, want 1", len(fake.updated))
	}
	if fake.updated[0].Properties.EnableIPForwarding == nil || !*fake.updated[0].Properties.EnableIPForwarding {
		t.Errorf("EnableIPForwarding was not set true in the write-back")
	}
}

// TestEnableIPForwarding_NoPropertiesRefuses pins that an interface with no
// Properties refuses rather than nil-dereferencing.
func TestEnableIPForwarding_NoPropertiesRefuses(t *testing.T) {
	c := &nicClient{interfaces: &fakeInterfaces{getResp: armnetwork.Interface{Name: to.Ptr("nic-a")}}}
	if err := c.EnableIPForwarding(context.Background(), "rg", "nic-a"); err == nil {
		t.Fatal("expected a refusal when the interface has no properties")
	}
}

// TestNewNICClient_SecondIdentityNeedsNoEnvironment pins that naming a
// second identity for the interface calls works in a pod whose
// environment carries no AZURE_TENANT_ID or AZURE_FEDERATED_TOKEN_FILE,
// which is every pod this operator runs in: nothing on OpenShift injects
// them into it.
func TestNewNICClient_SecondIdentityNeedsNoEnvironment(t *testing.T) {
	t.Setenv("AZURE_TENANT_ID", "")
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", "")

	if _, err := NewNICClient("subscription", "client-id", "tenant-id", nil); err != nil {
		t.Fatalf("NewNICClient: %v", err)
	}
}

// TestNewNICClient_SecondIdentityWithoutTenantRefuses pins that a second
// identity with no tenant to find it in is refused when the client is
// built, rather than at the first interface call.
func TestNewNICClient_SecondIdentityWithoutTenantRefuses(t *testing.T) {
	t.Setenv("AZURE_TENANT_ID", "")
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", "")

	if _, err := NewNICClient("subscription", "client-id", "", nil); err == nil {
		t.Fatal("NewNICClient: got nil error, want one naming the missing tenant")
	}
}
