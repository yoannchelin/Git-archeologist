// Package retrieve combines three signals to find the symbols most relevant
// to a natural-language question:
//
//  1. Vector similarity  — semantic match against doc/comments.
//     Catches "where do we charge users?" -> ChargeCustomer().
//
//  2. Lexical (FTS5)     — exact-token match against names/signatures.
//     Catches "ChargeCustomer" or "redis pool" when wording is precise.
//
//  3. Graph expansion    — pull callers/callees of the top hits.
//     This is what RAG misses: once we've found ChargeCustomer, the answer
//     usually requires its callers (the API handler) and its callees (the
//     Stripe client). Expansion of 1–2 hops typically lifts answer quality
//     more than embedding a bigger model would.
//
// We then re-rank with a weighted score and cap at the LLM's budget.
package retrieve

import (
	"context"
	"fmt"
	"strings"

	"github.com/yoannchl/git-archaeologist/internal/llm"
	"github.com/yoannchl/git-archaeologist/internal/store"
)

// Result is one retrieved symbol with provenance.
type Result struct {
	Symbol   store.Symbol
	File     *store.File
	Score    float32
	Reasons  []string // human-readable: "vector(0.74)", "fts", "called-by:X"
}

// Options controls retrieval breadth.
type Options struct {
	TopVector   int     // how many vector hits to seed with (default 20)
	TopFTS      int     // how many FTS hits to seed with (default 20)
	ExpandHops  int     // graph expansion depth (default 1)
	MaxResults  int     // final cap (default 25)
	WeightVec   float32 // default 1.0
	WeightFTS   float32 // default 0.7
	WeightGraph float32 // default 0.3
}

// DefaultOptions returns the recommended Options.
func DefaultOptions() Options {
	return Options{
		TopVector: 20, TopFTS: 20, ExpandHops: 1, MaxResults: 25,
		WeightVec: 1.0, WeightFTS: 0.7, WeightGraph: 0.3,
	}
}

// Query runs hybrid retrieval for a natural-language question.
//
// The flow is:
//
//	embed(q) -> vector top-K  ─┐
//	fts5(q)  -> lexical top-K ─┼─ pool  ─> graph expand ─> rerank ─> top-N
//
// Graph expansion follows incoming `calls` edges (callers of a hit) because
// for "where is X handled?" the caller is usually more informative than the
// callee.
func Query(
	ctx context.Context,
	s *store.Store,
	client *llm.Client,
	q string,
	opt Options,
) ([]Result, error) {
	if opt.TopVector == 0 {
		opt = DefaultOptions()
	}
	pool := make(map[int64]*Result)

	// --- 1. Vector ---
	qvec, err := client.Embed(ctx, q)
	if err == nil {
		hits, err := s.NearestNeighbors(qvec, opt.TopVector)
		if err == nil {
			for _, h := range hits {
				addHit(pool, h.SymbolID, h.Score*opt.WeightVec,
					fmt.Sprintf("vector(%.2f)", h.Score))
			}
		}
	}
	// We deliberately don't fail on embed error — we still have FTS to fall
	// back on, and a partial answer is better than none.

	// --- 2. FTS ---
	ftsQ := buildFTSQuery(q)
	if ftsQ != "" {
		ftsHits, err := s.SearchFTS(ftsQ, opt.TopFTS)
		if err == nil {
			// FTS doesn't return a normalised score, so we synthesise one
			// using rank position: top hit gets 1.0, decaying linearly.
			for i, sym := range ftsHits {
				rankScore := 1.0 - float32(i)/float32(len(ftsHits)+1)
				addHit(pool, sym.ID, rankScore*opt.WeightFTS, "fts")
			}
		}
	}

	// --- 3. Graph expansion ---
	// Take the current top-10 seeds, fan in by `calls` (callers).
	seeds := topNIDs(pool, 10)
	for _, sid := range seeds {
		callers, _ := s.Neighbors(sid, "calls", opt.ExpandHops, false /* incoming */)
		for _, cid := range callers {
			addHit(pool, cid, opt.WeightGraph, fmt.Sprintf("called-by-seed:%d", sid))
		}
		// Also pull implementations of interfaces in the seed set: if the
		// seed is an interface, "where is it used" includes its implementers.
		impls, _ := s.Neighbors(sid, "implements", 1, false)
		for _, iid := range impls {
			addHit(pool, iid, opt.WeightGraph, fmt.Sprintf("implements-seed:%d", sid))
		}
	}

	// --- 4. Hydrate + rank ---
	// Pre-fetch recently-modified files for freshness scoring. Failure here
	// is non-fatal: we just skip the boost.
	recentFiles, _ := s.RecentFileIDs(30)

	out := make([]Result, 0, len(pool))
	for _, r := range pool {
		sym, err := s.GetSymbolByID(r.Symbol.ID)
		if err != nil || sym == nil {
			continue
		}
		r.Symbol = *sym
		if sym.FileID != 0 {
			if f, err := s.GetFileByID(sym.FileID); err == nil {
				r.File = f
			}
		}
		// Strong preference: skip generated/test files unless they're the
		// only hits we have. For onboarding, hand-written code is the answer.
		if r.File != nil && (r.File.IsGenerated || r.File.IsTest) {
			r.Score *= 0.4
		}
		// Freshness boost: recently-committed symbols are likely more
		// relevant for "where does X happen?" in an active codebase.
		if r.File != nil && recentFiles[r.File.ID] {
			r.Score += 0.15
			r.Reasons = append(r.Reasons, "recent")
		}
		out = append(out, *r)
	}
	sortResultsDesc(out)
	if len(out) > opt.MaxResults {
		out = out[:opt.MaxResults]
	}
	return out, nil
}

func addHit(pool map[int64]*Result, id int64, score float32, reason string) {
	if r, ok := pool[id]; ok {
		r.Score += score
		r.Reasons = append(r.Reasons, reason)
		return
	}
	pool[id] = &Result{
		Symbol:  store.Symbol{ID: id},
		Score:   score,
		Reasons: []string{reason},
	}
}

// topNIDs returns the IDs of the top N entries by score, used as seeds for
// graph expansion. We do a tiny sort over the pool rather than maintaining
// a heap; pool size at this stage is < ~50.
func topNIDs(pool map[int64]*Result, n int) []int64 {
	all := make([]idScore, 0, len(pool))
	for id, r := range pool {
		all = append(all, idScore{id, r.Score})
	}
	sortKVDesc(all)
	if n > len(all) {
		n = len(all)
	}
	out := make([]int64, n)
	for i := 0; i < n; i++ {
		out[i] = all[i].id
	}
	return out
}

// idScore pairs a symbol id with a retrieval score. Internal to the package.
type idScore struct {
	id    int64
	score float32
}

// buildFTSQuery sanitises a natural-language question into something FTS5
// accepts. We:
//   - strip punctuation that confuses the tokenizer,
//   - drop stop-words (we don't need "the where is"),
//   - quote each remaining term with OR between them (broad recall).
//
// We accept the recall hit because the rerank step picks up precision.
func buildFTSQuery(q string) string {
	stop := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"where": true, "what": true, "how": true, "why": true, "which": true,
		"in": true, "on": true, "at": true, "of": true, "for": true, "to": true,
		"and": true, "or": true, "do": true, "does": true, "did": true,
		"i": true, "we": true, "you": true, "this": true, "that": true,
	}
	var tokens []string
	for _, raw := range strings.FieldsFunc(q, func(r rune) bool {
		return !(r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	}) {
		w := strings.ToLower(raw)
		if len(w) < 2 || stop[w] {
			continue
		}
		// FTS5 treats unquoted text as a phrase; quote each term to be safe.
		tokens = append(tokens, fmt.Sprintf("\"%s\"*", w))
	}
	if len(tokens) == 0 {
		return ""
	}
	return strings.Join(tokens, " OR ")
}
