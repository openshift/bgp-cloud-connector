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
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestStart_TriggersWhenBundleIsPublished(t *testing.T) {
	dir := t.TempDir()

	triggered := make(chan struct{})
	w := testWatcher(dir, func() { close(triggered) })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	startErr := make(chan error, 1)
	go func() {
		if err := w.Start(ctx); err != nil {
			startErr <- err
		}
	}()

	// Give the watcher a moment to register before publishing.
	time.Sleep(50 * time.Millisecond)
	publishData(t, dir, "..2026_01_01")

	select {
	case <-triggered:
	case err := <-startErr:
		t.Fatalf("Start failed before onChange fired: %v", err)
	case <-ctx.Done():
		t.Fatal("onChange did not fire although the bundle was published")
	}
}

func TestStart_DoesNotTriggerOnUnrelatedFile(t *testing.T) {
	dir := t.TempDir()

	triggered := make(chan struct{})
	w := testWatcher(dir, func() { close(triggered) })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	startErr := make(chan error, 1)
	go func() {
		if err := w.Start(ctx); err != nil {
			startErr <- err
		}
	}()

	// Give the watcher a moment to register before writing.
	time.Sleep(50 * time.Millisecond)
	publishUnrelatedData(t, dir)

	select {
	case <-triggered:
		t.Fatal("onChange fired for an unrelated file event")
	case err := <-startErr:
		t.Fatalf("Start failed: %v", err)
	case <-ctx.Done():
		// Expected: the unrelated write did not fire onChange before the timeout.
	}
}

// publishData simulates the kubelet publishing a new projected-volume version.
// Writes a fresh data directory and swaps the "..data" symlink to point at it.
func publishData(t *testing.T, dir, version string) {
	t.Helper()

	dataDir := filepath.Join(dir, version)
	bundlePath := filepath.Join(dataDir, "ca-bundle.crt")
	tmpLink := filepath.Join(dir, "..data_tmp")
	dataLink := filepath.Join(dir, "..data")

	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data version: %v", err)
	}
	if err := os.WriteFile(bundlePath, []byte("bundle"), 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	// Swap: create the link under a temp name, then rename it onto "..data".
	if err := os.Symlink(version, tmpLink); err != nil {
		t.Fatalf("symlink tmp: %v", err)
	}
	if err := os.Rename(tmpLink, dataLink); err != nil {
		t.Fatalf("rename onto ..data: %v", err)
	}
}

// publishUnrelatedData writes a file unrelated to the bundle into the watched directory.
func publishUnrelatedData(t *testing.T, dir string) {
	t.Helper()

	filePath := filepath.Join(dir, "unrelated.txt")

	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("write unrelated file: %v", err)
	}
}

func testWatcher(dir string, onChange func()) *Watcher {
	return &Watcher{
		dir:      dir,
		onChange: onChange,
		log:      logr.Discard(),
	}
}
