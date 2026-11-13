package gcp_e2e

import (
	"testing"

	ginkgotypes "github.com/onsi/ginkgo/v2/types"
)

// Ginkgo puts the BeforeSuite node's own report in SpecReports, so a
// failed setup must not count as a spec having run, or the setup
// diagnostics are never printed.
func TestSpecRan(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reports ginkgotypes.SpecReports
		want    bool
	}{{
		name: "BeforeSuite failed, no spec ran",
		reports: ginkgotypes.SpecReports{
			{LeafNodeType: ginkgotypes.NodeTypeBeforeSuite, State: ginkgotypes.SpecStateFailed},
			{LeafNodeType: ginkgotypes.NodeTypeIt, State: ginkgotypes.SpecStateSkipped},
		},
		want: false,
	}, {
		name: "BeforeSuite passed, a spec failed",
		reports: ginkgotypes.SpecReports{
			{LeafNodeType: ginkgotypes.NodeTypeBeforeSuite, State: ginkgotypes.SpecStatePassed},
			{LeafNodeType: ginkgotypes.NodeTypeIt, State: ginkgotypes.SpecStateFailed},
		},
		want: true,
	}, {
		name: "BeforeSuite passed, every spec skipped",
		reports: ginkgotypes.SpecReports{
			{LeafNodeType: ginkgotypes.NodeTypeBeforeSuite, State: ginkgotypes.SpecStatePassed},
			{LeafNodeType: ginkgotypes.NodeTypeIt, State: ginkgotypes.SpecStateSkipped},
		},
		want: false,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := specRan(tc.reports); got != tc.want {
				t.Errorf("specRan = %v, want %v", got, tc.want)
			}
		})
	}
}
