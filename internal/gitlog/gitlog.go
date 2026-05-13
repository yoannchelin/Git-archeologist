// Package gitlog ingests commit history into the index.
//
// We care about three things from git:
//  1. recent commits (to know what's hot),
//  2. churn per file (proxy for risk + complexity),
//  3. who modifies what (de-facto owners).
//
// We use go-git to stay pure-Go (no shelling out to /usr/bin/git). The
// trade-off: go-git is ~3× slower than C git on huge repos. For onboarding
// scenarios that's fine; we cap traversal at MaxCommits.
package gitlog

import (
	"fmt"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/yoannchl/git-archaeologist/internal/store"
)

// Ingest walks at most maxCommits from HEAD and records commits + per-file churn.
//
// The function is best-effort: it skips commits that fail to expose stats
// (e.g. merge commits with no patch in go-git's model) rather than aborting.
func Ingest(repoRoot string, s *store.Store, maxCommits int) (int, error) {
	repo, err := git.PlainOpen(repoRoot)
	if err != nil {
		return 0, fmt.Errorf("open repo: %w", err)
	}
	head, err := repo.Head()
	if err != nil {
		return 0, fmt.Errorf("head: %w", err)
	}
	iter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	if err != nil {
		return 0, fmt.Errorf("log: %w", err)
	}

	// Pre-fetch file_id by path so we can join churn back to our `files` table
	// without a query per commit.
	pathToFileID, err := loadFileIndex(s)
	if err != nil {
		return 0, err
	}

	tx, err := s.DB().Begin()
	if err != nil {
		return 0, err
	}
	commitStmt, err := tx.Prepare(`
		INSERT INTO commits(hash, author, email, ts, subject)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(hash) DO NOTHING`)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	defer commitStmt.Close()
	churnStmt, err := tx.Prepare(`
		INSERT INTO file_commits(file_id, commit_hash, added, deleted)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(file_id, commit_hash) DO UPDATE SET
			added = excluded.added, deleted = excluded.deleted`)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	defer churnStmt.Close()

	count := 0
	err = iter.ForEach(func(c *object.Commit) error {
		if maxCommits > 0 && count >= maxCommits {
			return storer.ErrStop
		}
		count++
		if _, err := commitStmt.Exec(
			c.Hash.String(),
			c.Author.Name, c.Author.Email,
			c.Author.When.Unix(),
			firstLine(c.Message),
		); err != nil {
			return err
		}
		// Stats are expensive (require diffing against parent). We only
		// compute them for non-merge commits; merges inflate churn artificially.
		if c.NumParents() > 1 {
			return nil
		}
		stats, err := c.Stats()
		if err != nil {
			return nil // best-effort
		}
		for _, st := range stats {
			fid, ok := pathToFileID[st.Name]
			if !ok {
				continue // file not in our Go index (e.g. .md, vendored)
			}
			if _, err := churnStmt.Exec(fid, c.Hash.String(), st.Addition, st.Deletion); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil && err != storer.ErrStop {
		_ = tx.Rollback()
		return count, fmt.Errorf("walk: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return count, err
	}
	return count, nil
}

// HotFiles returns the top-N files by raw churn over the recorded history,
// joined with their LOC for a quick "risk" view.
func HotFiles(s *store.Store, limit int) ([]HotFile, error) {
	rows, err := s.DB().Query(`
		SELECT f.path, f.package, f.loc,
		       COALESCE(SUM(fc.added + fc.deleted), 0) AS churn,
		       COUNT(DISTINCT fc.commit_hash) AS commits
		FROM files f
		LEFT JOIN file_commits fc ON fc.file_id = f.id
		GROUP BY f.id
		ORDER BY churn DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HotFile
	for rows.Next() {
		var h HotFile
		if err := rows.Scan(&h.Path, &h.Package, &h.LOC, &h.Churn, &h.Commits); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// HotFile is one row of HotFiles output.
type HotFile struct {
	Path    string
	Package string
	LOC     int
	Churn   int
	Commits int
}

func loadFileIndex(s *store.Store) (map[string]int64, error) {
	rows, err := s.DB().Query(`SELECT id, path FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64)
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		out[p] = id
	}
	return out, rows.Err()
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
