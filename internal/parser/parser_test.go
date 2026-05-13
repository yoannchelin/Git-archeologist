package parser_test

import (
	"path/filepath"
	"runtime"
	"testing"

	"github.com/yoannchl/git-archaeologist/internal/parser"
	"github.com/yoannchl/git-archaeologist/internal/store"
)

// TestParseSample indexes the sample repo and asserts the basic shape of
// what comes out. This is the safety net for parser regressions: if any of
// these counts change, we want to know on purpose, not by accident.
func TestParseSample(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "testdata", "sample")

	tmp := t.TempDir()
	s, err := store.Open(tmp)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	stats, err := parser.Parse(root, s)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if stats.Packages < 1 {
		t.Errorf("expected >=1 packages, got %d", stats.Packages)
	}
	if stats.Files < 1 {
		t.Errorf("expected >=1 files, got %d", stats.Files)
	}
	// We expect at least: Provider (interface), StripeProvider, PaypalProvider,
	// ChargeCustomer, two Charge methods, two Refund methods.
	if stats.Symbols < 7 {
		t.Errorf("expected >=7 symbols, got %d", stats.Symbols)
	}
	// ChargeCustomer calls p.Charge — that's at least one call edge.
	if stats.CallEdges < 1 {
		t.Errorf("expected >=1 call edges, got %d", stats.CallEdges)
	}
	// Both providers implement Provider (T or *T).
	if stats.ImplEdges < 2 {
		t.Errorf("expected >=2 impl edges, got %d", stats.ImplEdges)
	}

	// Quick FTS round-trip.
	hits, err := s.SearchFTS(`"charge"*`, 10)
	if err != nil {
		t.Fatalf("fts: %v", err)
	}
	if len(hits) == 0 {
		t.Error("FTS returned 0 hits for 'charge'")
	}
}
