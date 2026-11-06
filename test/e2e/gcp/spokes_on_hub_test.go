package gcp_e2e

import (
	"testing"

	"google.golang.org/api/networkconnectivity/v1"
)

// A spoke on another hub whose name merely contains this one's must not
// count, or the spoke-count and spoke-gone checks depend on hubs that
// are not ours.
func TestSpokesOnHub(t *testing.T) {
	spokes := []*networkconnectivity.Spoke{
		{Name: "ours", Hub: "projects/p/locations/global/hubs/e2e-hub"},
		{Name: "longer", Hub: "projects/p/locations/global/hubs/e2e-hub-2"},
		{Name: "shorter", Hub: "projects/p/locations/global/hubs/e2e"},
	}
	// The operator accepts the hub as a name or as a full resource path,
	// so the suite has to match spokes for a profile written either way.
	for _, hubName := range []string{"e2e-hub", "projects/p/locations/global/hubs/e2e-hub"} {
		got := spokesOnHub(spokes, hubName)
		if len(got) != 1 || got[0].Name != "ours" {
			var names []string
			for _, s := range got {
				names = append(names, s.Name)
			}
			t.Errorf("hub %q: got spokes %v, want only [ours]", hubName, names)
		}
	}
}
