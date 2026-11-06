package gcp_e2e

import (
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func session(node, peer, status string) unstructured.Unstructured {
	return unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"node": node, "peer": peer, "bgpStatus": status},
	}}
}

// Each router node peers with both interfaces of the Cloud Router's
// redundant pair, so a node with one session Established and the other
// down is half the redundancy missing, not a pass.
func TestSessionsMissing(t *testing.T) {
	neighbours := map[string]bool{"10.0.128.5": true, "10.0.128.6": true}
	nodes := []string{"worker-a", "worker-b"}

	for _, tc := range []struct {
		name     string
		sessions []unstructured.Unstructured
		want     []string
	}{{
		name: "every node Established to both interfaces",
		sessions: []unstructured.Unstructured{
			session("worker-a", "10.0.128.5", "Established"),
			session("worker-a", "10.0.128.6", "Established"),
			session("worker-b", "10.0.128.5", "Established"),
			session("worker-b", "10.0.128.6", "Established"),
		},
	}, {
		name: "one node's second session down",
		sessions: []unstructured.Unstructured{
			session("worker-a", "10.0.128.5", "Established"),
			session("worker-a", "10.0.128.6", "Established"),
			session("worker-b", "10.0.128.5", "Established"),
			session("worker-b", "10.0.128.6", "Active"),
		},
		want: []string{"worker-b -> 10.0.128.6"},
	}, {
		name: "a session to an address that is not a neighbour does not count",
		sessions: []unstructured.Unstructured{
			session("worker-a", "10.0.128.5", "Established"),
			session("worker-a", "10.0.128.9", "Established"),
			session("worker-b", "10.0.128.5", "Established"),
			session("worker-b", "10.0.128.6", "Established"),
		},
		want: []string{"worker-a -> 10.0.128.6"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionsMissing(tc.sessions, nodes, neighbours)
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
