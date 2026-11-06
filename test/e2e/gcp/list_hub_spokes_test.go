package gcp_e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/api/networkconnectivity/v1"
	"google.golang.org/api/option"
)

// A shared CI project can hold more spokes than one page, and this
// cluster's can be on any of them. Reading only the first page would
// let the "spoke has gone" check pass while the spoke is still there.
func TestListHubSpokesReadsEveryPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := &networkconnectivity.ListSpokesResponse{}
		switch r.URL.Query().Get("pageToken") {
		case "":
			resp.Spokes = []*networkconnectivity.Spoke{{Name: "theirs", Hub: "projects/p/locations/global/hubs/other"}}
			resp.NextPageToken = "page-2"
		case "page-2":
			resp.Spokes = []*networkconnectivity.Spoke{{Name: "ours", Hub: "projects/p/locations/global/hubs/e2e-hub"}}
		default:
			t.Errorf("unexpected page token %q", r.URL.Query().Get("pageToken"))
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("encoding: %v", err)
		}
	}))
	defer srv.Close()

	svc, err := networkconnectivity.NewService(context.Background(),
		option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("building the client: %v", err)
	}
	got, err := listHubSpokes(context.Background(), svc, "p", "e2e-hub")
	if err != nil {
		t.Fatalf("listHubSpokes: %v", err)
	}
	if len(got) != 1 || got[0].Name != "ours" {
		var names []string
		for _, s := range got {
			names = append(names, s.Name)
		}
		t.Errorf("got spokes %v, want [ours] from the second page", names)
	}
}
