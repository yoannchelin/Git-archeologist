// Package index orchestrates a full index build: parser → git → embeddings.
//
// Keeping the orchestration here (instead of in cmd/) lets the MCP server
// trigger reindexing without spawning a subprocess.
package index

import (
	"context"
	"fmt"
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
	MaxCommits     int  // cap git traversal (default 5000)
}

// DefaultOptions returns sensible defaults for an onboarding index.
func DefaultOptions() Options {
	return Options{WithGit: true, WithEmbeddings: true, MaxCommits: 5000}
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
	pstats, err := parser.Parse(repoRoot, s)
	if err != nil {
		return report, fmt.Errorf("parse: %w", err)
	}
	report.ParseStats = pstats
	report.ParseErrors = pstats.Errors
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

	if err := s.SetMeta("last_index", time.Now().Format(time.RFC3339)); err != nil {
		return report, err
	}
	report.Duration = time.Since(start)
	return report, nil
}
