// Package index orchestrates a full index build: parser → git → embeddings.
//
// Keeping the orchestration here (instead of in cmd/) lets the MCP server
// trigger reindexing without spawning a subprocess.
package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yoannchl/git-archaeologist/internal/embed"
	"github.com/yoannchl/git-archaeologist/internal/gitlog"
	"github.com/yoannchl/git-archaeologist/internal/llm"
	"github.com/yoannchl/git-archaeologist/internal/parser"
	"github.com/yoannchl/git-archaeologist/internal/store"
)

// Options controls what an index build does.
type Options struct {
	WithGit        bool // include git history (default true)
	WithEmbeddings bool // include vector embeddings (default true)
	WithTests      bool // include _test.go files (default false; ~2× slower parse)
	Fast           bool // skip type-checking deps — no call/impl edges, ~3-5× faster
	MaxCommits     int  // cap git traversal (default 5000)
}

// DefaultOptions returns sensible defaults for an onboarding index.
func DefaultOptions() Options {
	return Options{WithGit: true, WithEmbeddings: true, WithTests: false, Fast: false, MaxCommits: 5000}
}

// Report summarises an index run.
type Report struct {
	ParseStats   *parser.Stats
	Commits      int
	Embedded     int
	Duration     time.Duration
	ParseErrors  []string
}

// Build runs a full or partial index against the repo rooted at repoRoot.
//
// llmClient may be nil when WithEmbeddings is false. progress is forwarded
// to the embedding pipeline; pass nil if you don't need it.
func Build(
	ctx context.Context,
	repoRoot string,
	s *store.Store,
	llmClient *llm.Client,
	opt Options,
	progress func(stage string, done, total int),
) (*Report, error) {
	start := time.Now()
	report := &Report{}

	if progress != nil {
		progress("parse", 0, 1)
	}

	cfg := parser.ParseConfig{WithTests: opt.WithTests, Fast: opt.Fast}

	// Detect go.work: index each module directory so multi-module workspaces
	// (e.g. Kubernetes) get full coverage instead of just the root module.
	moduleDirs := parseGoWorkDirs(repoRoot)
	if len(moduleDirs) == 0 {
		moduleDirs = []string{""} // single module: LoadDir defaults to repoRoot
	}

	pstats := &parser.Stats{}
	for _, modDir := range moduleDirs {
		mcfg := cfg
		mcfg.LoadDir = modDir
		ms, err := parser.Parse(repoRoot, s, mcfg)
		if err != nil {
			return report, fmt.Errorf("parse: %w", err)
		}
		pstats.Packages += ms.Packages
		pstats.Files += ms.Files
		pstats.Symbols += ms.Symbols
		pstats.CallEdges += ms.CallEdges
		pstats.ImplEdges += ms.ImplEdges
		pstats.ImportEdges += ms.ImportEdges
		pstats.EmbedEdges += ms.EmbedEdges
		pstats.Errors = append(pstats.Errors, ms.Errors...)
	}
	report.ParseStats = pstats
	report.ParseErrors = pstats.Errors

	// TypeScript support: parse .ts/.tsx files if present. Non-fatal if the
	// repo has none — ParseTS returns an empty Stats in that case.
	if tsStats, err := parser.ParseTS(repoRoot, s); err != nil {
		report.ParseErrors = append(report.ParseErrors, "ts: "+err.Error())
	} else {
		report.ParseStats.Files += tsStats.Files
		report.ParseStats.Symbols += tsStats.Symbols
		report.ParseStats.CallEdges += tsStats.CallEdges
		report.ParseStats.ImportEdges += tsStats.ImportEdges
		report.ParseErrors = append(report.ParseErrors, tsStats.Errors...)
	}

	if progress != nil {
		progress("parse", 1, 1)
	}

	if opt.WithGit {
		if progress != nil {
			progress("git", 0, 1)
		}
		n, err := gitlog.Ingest(repoRoot, s, opt.MaxCommits)
		if err != nil {
			// Not fatal: a brand-new repo may not have history yet.
			report.ParseErrors = append(report.ParseErrors, "git: "+err.Error())
		}
		report.Commits = n
		if progress != nil {
			progress("git", 1, 1)
		}
	}

	if opt.WithEmbeddings {
		if llmClient == nil {
			return report, fmt.Errorf("embeddings requested but llm client is nil")
		}
		var lastDone int
		err := embed.Run(ctx, s, llmClient, repoRoot, func(done, total int) {
			lastDone = done
			if progress != nil {
				progress("embed", done, total)
			}
		})
		if err != nil {
			return report, fmt.Errorf("embed: %w", err)
		}
		report.Embedded = lastDone
	}

	// PageRank: computed after all edges are in place. Non-fatal so a graph
	// with no edges (e.g. --no-embed on a fresh TS repo) doesn't abort the build.
	if err := s.ComputePageRank(); err != nil {
		report.ParseErrors = append(report.ParseErrors, "pagerank: "+err.Error())
	}

	if err := s.SetMeta("last_index", time.Now().Format(time.RFC3339)); err != nil {
		return report, err
	}
	report.Duration = time.Since(start)
	return report, nil
}

// parseGoWorkDirs reads go.work and returns the absolute path for each `use`
// directive. Returns nil when no go.work exists or it cannot be read.
func parseGoWorkDirs(repoRoot string) []string {
	data, err := os.ReadFile(filepath.Join(repoRoot, "go.work"))
	if err != nil {
		return nil
	}
	var dirs []string
	inUse := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "use (":
			inUse = true
		case inUse && line == ")":
			inUse = false
		case inUse && line != "" && !strings.HasPrefix(line, "//"):
			dirs = append(dirs, filepath.Join(repoRoot, filepath.FromSlash(line)))
		case strings.HasPrefix(line, "use ") && !strings.Contains(line, "("):
			p := strings.TrimSpace(strings.TrimPrefix(line, "use "))
			dirs = append(dirs, filepath.Join(repoRoot, filepath.FromSlash(p)))
		}
	}
	return dirs
}
