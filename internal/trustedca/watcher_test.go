/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package trustedca

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

// publishData simulates the kubelet publishing a new projected-volume version:
// it writes a fresh timestamped data directory and atomically swaps the "..data"
// symlink to point at it.
func publishData(t *testing.T, dir, version string) {
	t.Helper()
	dataDir := filepath.Join(dir, version)
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data version: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "ca-bundle.crt"), []byte("bundle"), 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	// Atomic swap: create the link under a temp name, then rename it onto "..data".
	tmpLink := filepath.Join(dir, "..data_tmp")
	if err := os.Symlink(version, tmpLink); err != nil {
		t.Fatalf("symlink tmp: %v", err)
	}
	if err := os.Rename(tmpLink, filepath.Join(dir, dataLink)); err != nil {
		t.Fatalf("rename onto ..data: %v", err)
	}
}

func testWatcher(dir string, onChange func()) *Watcher {
	return &Watcher{
		dir:      dir,
		onChange: onChange,
		log:      logr.Discard(),
	}
}

func TestStart_TriggersWhenBundleIsPublished(t *testing.T) {
	dir := t.TempDir()

	triggered := make(chan struct{})
	w := testWatcher(dir, func() { close(triggered) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	// Give the watcher a moment to register before publishing.
	time.Sleep(50 * time.Millisecond)
	publishData(t, dir, "..2026_01_01")

	select {
	case <-triggered:
	case <-ctx.Done():
		t.Fatal("onChange did not fire although the bundle was published")
	}
}

func TestStart_DoesNotTriggerOnUnrelatedFile(t *testing.T) {
	dir := t.TempDir()

	var called atomic.Bool
	w := testWatcher(dir, func() { called.Store(true) })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { _ = w.Start(ctx); close(done) }()

	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}

	<-done
	if called.Load() {
		t.Error("onChange fired for an unrelated file event")
	}
}
