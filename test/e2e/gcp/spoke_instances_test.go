package gcp_e2e

import (
	"slices"
	"testing"

	"google.golang.org/api/networkconnectivity/v1"
)

// Spokes name their instances by URL, and the operator may write either
// the full https form or the bare resource path, so only the name after
// /instances/ is compared. A spoke with no linked instances contributes
// nothing rather than failing.
func TestSpokeInstanceNames(t *testing.T) {
	spokes := []*networkconnectivity.Spoke{
		{
			Name: "spoke-0",
			LinkedRouterApplianceInstances: &networkconnectivity.LinkedRouterApplianceInstances{
				Instances: []*networkconnectivity.RouterApplianceInstance{
					{VirtualMachine: "https://www.googleapis.com/compute/v1/projects/p/zones/z/instances/worker-b"},
					{VirtualMachine: "projects/p/zones/z/instances/worker-c"},
				},
			},
		},
		{Name: "spoke-1"},
	}
	got := spokeInstanceNames(spokes)
	slices.Sort(got)
	want := []string{"worker-b", "worker-c"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
