package store_test

import (
	"math/rand"
	"testing"

	"github.com/yoannchl/git-archaeologist/internal/store"
)

func TestHNSWNearestNeighbors(t *testing.T) {
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	// Insert a symbol so embeddings have something to reference.
	batch, err := s.Begin()
	if err != nil {
		t.Fatal(err)
	}
	id, err := batch.PutSymbol(store.Symbol{
		Kind: "func", Name: "foo", Qualified: "pkg.foo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.Commit(); err != nil {
		t.Fatal(err)
	}

	// Build a 768-dim vector and store it.
	vec := make([]float32, 768)
	for i := range vec {
		vec[i] = rand.Float32()
	}
	if err := s.PutEmbedding(id, vec, "nomic-embed-text"); err != nil {
		t.Fatalf("PutEmbedding: %v", err)
	}

	// Nearest-neighbor search should use HNSW and return the symbol.
	hits, err := s.NearestNeighbors(vec, 5)
	if err != nil {
		t.Fatalf("NearestNeighbors: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("expected >=1 hit, got 0")
	}
	if hits[0].SymbolID != id {
		t.Errorf("expected symbol %d, got %d", id, hits[0].SymbolID)
	}
	// Self-match should be very close to 1.0.
	if hits[0].Score < 0.99 {
		t.Errorf("self-match score too low: %f", hits[0].Score)
	}
	t.Logf("HNSW hit: symbol=%d score=%.4f", hits[0].SymbolID, hits[0].Score)
}
