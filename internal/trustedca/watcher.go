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
	"fmt"
	"path/filepath"

	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	// bundleDir is where the trusted CA ConfigMap is mounted into the manager,
	// replacing the image's trust store. The mount is a directory (not a
	// subPath), so the kubelet refreshes it in place when the bundle rotates.
	bundleDir = "/etc/pki/tls/certs"

	// dataLink is the symlink the kubelet atomically swaps to publish a new
	// version of a projected volume. Its appearance is the single reliable
	// signal that the mounted bundle content changed.
	dataLink = "..data"
)

// Watcher watches the mounted trusted CA bundle directory and calls onChange
// when the kubelet publishes a new bundle, so the manager can restart and
// rebuild its certificate pool from the rotated bundle. It is a manager
// Runnable.
type Watcher struct {
	dir      string
	onChange func()
	log      logr.Logger
}

// New returns a Watcher for the mounted trusted CA bundle directory.
func New(ctx context.Context, onChange func()) *Watcher {
	return &Watcher{
		dir:      bundleDir,
		onChange: onChange,
		log:      logr.FromContextOrDiscard(ctx),
	}
}

// SetupWithManager registers the Watcher so the manager runs and stops it.
func (w *Watcher) SetupWithManager(mgr ctrl.Manager) error {
	return mgr.Add(w)
}

// Start watches the trusted CA bundle directory until the bundle rotates or ctx
// is cancelled. It satisfies manager.Runnable.
func (w *Watcher) Start(ctx context.Context) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to create fsnotify watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	if err := watcher.Add(w.dir); err != nil {
		return fmt.Errorf("failed to watch trusted CA bundle directory %s: %w", w.dir, err)
	}

	w.log.Info("Starting to watch trusted CA bundle directory", "dir", w.dir)

	for {
		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			// The kubelet swaps the "..data" symlink (an atomic rename into the
			// directory, reported as Create) to publish a new bundle version.
			if filepath.Base(event.Name) == dataLink && event.Op&(fsnotify.Create|fsnotify.Rename) != 0 {
				w.log.Info("trusted CA bundle changed, initiating shutdown to reload it")
				w.onChange()
				return nil
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			w.log.Error(err, "error watching trusted CA bundle directory")
		}
	}
}
