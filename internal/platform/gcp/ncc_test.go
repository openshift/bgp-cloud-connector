package gcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"google.golang.org/api/networkconnectivity/v1"
	"google.golang.org/api/option"
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

// TestSpokeMatches covers what a patch can change: the instances and the
// address each is reached on. Comparing only the instance self links would let
// a node keeping its instance while changing address read as nothing to do.
func TestSpokeMatches(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoke *networkconnectivity.Spoke
		nodes []RouterNode
		want  bool
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := spokeMatches(tc.spoke, tc.nodes); got != tc.want {
				t.Errorf("spokeMatches = %v, want %v", got, tc.want)
			}
		})
	}
}

// fakeSpokeServer serves one existing spoke as the NCC API does, and
// answers a patch as GCP does: a mask naming
// linkedRouterApplianceInstances.siteToSiteDataTransfer is refused with
// 400 whatever the value, because GCP fixes that setting when a spoke
// is created. Observed against a live spoke on 2026-11-06, both with
// the value unchanged and with it flipped. It records each patch's
// mask.
func fakeSpokeServer(t *testing.T, existing *networkconnectivity.Spoke) (*httptest.Server, *[]string) {
	t.Helper()
	var masks []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if err := json.NewEncoder(w).Encode(existing); err != nil {
				t.Errorf("encoding: %v", err)
			}
		case http.MethodPatch:
			mask := r.URL.Query().Get("updateMask")
			masks = append(masks, mask)
			if strings.Contains(mask, "siteToSiteDataTransfer") {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":400,"message":"changing field \"linked_router_appliance_instances.site_to_site_data_transfer\" is not allowed: invalid argument","status":"INVALID_ARGUMENT"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"done":true}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &masks
}

func fakeNCCClient(t *testing.T, srv *httptest.Server) *nccClient {
	t.Helper()
	svc, err := networkconnectivity.NewService(context.Background(),
		option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	return &nccClient{svc: svc, project: "proj", region: "us-east1"}
}

// A router node joining is the everyday change to a spoke. The patch
// that adds it must leave site-to-site data transfer out of its mask,
// or GCP refuses the whole patch and the node is never added. Observed
// on an OSD cluster when a fourth worker was labelled a router.
func TestReconcileSpokeAddsANodeWithoutNamingSiteToSite(t *testing.T) {
	srv, masks := fakeSpokeServer(t, spokeWith(false, appliance("worker-a", "10.0.1.4")))
	n := fakeNCCClient(t, srv)

	changed, err := n.ReconcileSpoke(context.Background(), "spoke-0", "hub",
		[]RouterNode{routerNode("worker-a", "10.0.1.4"), routerNode("worker-b", "10.0.1.5")}, false)
	if err != nil {
		t.Fatalf("ReconcileSpoke: %v", err)
	}
	if !changed {
		t.Error("ReconcileSpoke reported no change after adding a node")
	}
	if want := []string{"linkedRouterApplianceInstances.instances"}; !slices.Equal(*masks, want) {
		t.Errorf("patch masks = %q, want %q", *masks, want)
	}
}

// GCP will not change site-to-site data transfer on an existing spoke,
// so a spec that differs from the spoke is reported, not patched: no
// patch is sent for it, and the error says how to change it.
func TestReconcileSpokeReportsASiteToSiteItCannotApply(t *testing.T) {
	srv, masks := fakeSpokeServer(t, spokeWith(true, appliance("worker-a", "10.0.1.4")))
	n := fakeNCCClient(t, srv)

	_, err := n.ReconcileSpoke(context.Background(), "spoke-0", "hub",
		[]RouterNode{routerNode("worker-a", "10.0.1.4")}, false)
	if err == nil || !strings.Contains(err.Error(), "only when the spoke is created") {
		t.Errorf("ReconcileSpoke error = %v, want one saying the setting is chosen only when the spoke is created", err)
	}
	if len(*masks) != 0 {
		t.Errorf("sent patches %q; want none, since GCP refuses this change", *masks)
	}
}
