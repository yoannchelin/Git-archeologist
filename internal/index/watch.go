// watch.go wires the file watcher to incremental re-indexing.
//
// When a .go file is saved the flow is:
//   fsnotify event → debounce 500ms → DeletePackageData → ParsePackage → (re-embed)
//
// Only the changed package is re-parsed; other packages are untouched.
// Incoming call edges from other packages survive because symbol IDs are
// stable across upserts (ON CONFLICT … RETURNING id).
package index

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/yoannchl/git-archaeologist/internal/embed"
	"github.com/yoannchl/git-archaeologist/internal/llm"
	"github.com/yoannchl/git-archaeologist/internal/parser"
	"github.com/yoannchl/git-archaeologist/internal/store"
	"github.com/yoannchl/git-archaeologist/internal/watch"
)

// StartWatcher launches a background goroutine that incrementally re-indexes
// Go packages as their source files change.
//
// llmClient may be nil; if non-nil, affected symbols are re-embedded after
// each parse (in a separate goroutine so it doesn't block the watcher).
func StartWatcher(
	ctx context.Context,
	repoRoot string,
	s *store.Store,
	llmClient *llm.Client,
	logger *log.Logger,
) error {
	resolve := func(relPath string) (string, bool) {
		// Fast path: file already indexed.
		var pkg string
		if err := s.DB().QueryRow(
			`SELECT package FROM files WHERE path = ?`, relPath,
		).Scan(&pkg); err == nil {
			return pkg, true
		}
		// Slow path: new file — find siblings in the same directory.
		slash := strings.LastIndex(relPath, "/")
		dir := relPath
		if slash >= 0 {
			dir = relPath[:slash]
		}
		if err := s.DB().QueryRow(
			`SELECT package FROM files WHERE path LIKE ? LIMIT 1`, dir+"%.go",
		).Scan(&pkg); err == nil {
			return pkg, true
		}
		return "", false
	}

	handler := func(pkgPath string) {
		logger.Printf("[watch] re-indexing %s", pkgPath)
		if err := s.DeletePackageData(pkgPath); err != nil {
			logger.Printf("[watch] delete %s: %v", pkgPath, err)
			return
		}
		stats, err := parser.ParsePackage(repoRoot, pkgPath, s)
		if err != nil {
			logger.Printf("[watch] parse %s: %v", pkgPath, err)
			return
		}
		logger.Printf("[watch] %s: %d symbols, %d call edges", pkgPath, stats.Symbols, stats.CallEdges)
		if llmClient != nil {
			go func() {
				if err := embed.RunPackage(ctx, s, llmClient, repoRoot, pkgPath, nil); err != nil {
					logger.Printf("[watch] embed %s: %v", pkgPath, err)
				}
			}()
		}
	}

	w, err := watch.New(repoRoot, resolve, handler)
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	go func() {
		w.Run(ctx)
		_ = w.Close()
	}()
	logger.Printf("[watch] watching %s", repoRoot)
	return nil
}
