// Package watch debounces fsnotify file events to package-level callbacks.
//
// Why debounce at the package level?
// A single editor save often fires Write + Chmod, and gofmt may follow with
// another Write. Debouncing to 500ms collapses those into one re-index call.
// We key on the package path (not the file) so that saving two files in the
// same package back-to-back triggers only one re-parse.
package watch

import (
	"context"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const debounce = 500 * time.Millisecond

// PackageResolver maps a repo-relative .go file path to its Go import path.
// Returns ("", false) when the package is unknown (e.g. first save of a new file).
type PackageResolver func(relPath string) (pkgPath string, ok bool)

// Handler is called in its own goroutine after the debounce window closes.
// pkgPath is the Go import path of the package that needs re-indexing.
type Handler func(pkgPath string)

// Watcher watches a directory tree and debounces .go file changes.
type Watcher struct {
	fsw    *fsnotify.Watcher
	root   string
	resolve PackageResolver
	handle  Handler

	mu     sync.Mutex
	timers map[string]*time.Timer
}

// New creates a Watcher that recursively watches all non-vendor, non-hidden
// directories under root.
func New(root string, resolve PackageResolver, handle Handler) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		switch d.Name() {
		case ".git", ".archaeo", "vendor", "node_modules", "testdata":
			return filepath.SkipDir
		}
		if strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		return fsw.Add(path)
	})
	if err != nil {
		_ = fsw.Close()
		return nil, err
	}
	return &Watcher{
		fsw:    fsw,
		root:   root,
		resolve: resolve,
		handle:  handle,
		timers: make(map[string]*time.Timer),
	}, nil
}

// Run processes fsnotify events until ctx is cancelled.
// Call this in a goroutine; it blocks until done.
func (w *Watcher) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if !isGoSource(event.Name) {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			rel, err := filepath.Rel(w.root, event.Name)
			if err != nil {
				continue
			}
			rel = filepath.ToSlash(rel)
			pkg, ok := w.resolve(rel)
			if !ok {
				log.Printf("watch: package unknown for %s — skipping", rel)
				continue
			}
			w.schedule(pkg)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			log.Printf("watch: %v", err)
		}
	}
}

// Close stops the underlying fsnotify watcher.
func (w *Watcher) Close() error { return w.fsw.Close() }

// schedule arms (or resets) a debounce timer for pkg.
func (w *Watcher) schedule(pkg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.timers[pkg]; ok {
		t.Reset(debounce)
		return
	}
	w.timers[pkg] = time.AfterFunc(debounce, func() {
		w.mu.Lock()
		delete(w.timers, pkg)
		w.mu.Unlock()
		go w.handle(pkg)
	})
}

func isGoSource(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}
