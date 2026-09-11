package gcp_e2e

import (
	"context"
	"testing"
	"time"
)

// A section that hangs on an unanswering API must be cut off at its
// deadline, and must not stop the sections after it from printing.
func TestDumpSectionsBoundsEachSection(t *testing.T) {
	var ranAfter bool
	hang := func(ctx context.Context) { <-ctx.Done() }
	after := func(ctx context.Context) { ranAfter = ctx.Err() == nil }

	done := make(chan struct{})
	go func() {
		dumpSections(50*time.Millisecond, hang, after)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dumpSections did not return: a hung section blocked it")
	}
	if !ranAfter {
		t.Error("the section after the hung one did not run with a live context")
	}
}
