// vec.go registers a custom SQLite driver with two vector capabilities:
//
//  1. cosine_sim(a, b) scalar function — brute-force fallback, always available.
//  2. sqlite-vec HNSW via vec0 virtual tables — used automatically once the
//     vec_embeddings table is populated. Registered globally via Auto() so every
//     new connection gets the extension without any per-connection setup.
//
// Both coexist on the same connection: cosine_sim stays for FTS re-ranking and
// any path that bypasses the HNSW index; vec0 handles the ANN hot path.
package store

import (
	"database/sql"
	"encoding/binary"
	"math"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	sqlite3 "github.com/mattn/go-sqlite3"
)

func init() {
	// Register sqlite-vec globally so every new SQLite connection (including the
	// sqlite3_archaeo driver below) automatically loads the vec0 virtual-table
	// extension. Must be called before any connection is opened.
	sqlite_vec.Auto()

	sql.Register("sqlite3_archaeo", &sqlite3.SQLiteDriver{
		ConnectHook: func(conn *sqlite3.SQLiteConn) error {
			return conn.RegisterFunc("cosine_sim", cosineSim, true)
		},
	})
}

// normalizeVec returns a unit-length copy of v. Used to store normalized
// vectors in vec_embeddings so that L2 distance on vec0 equals cosine distance.
// Returns v unchanged if its magnitude is zero.
func normalizeVec(v []float32) []float32 {
	var sumSq float64
	for _, x := range v {
		sumSq += float64(x) * float64(x)
	}
	if sumSq == 0 {
		return v
	}
	mag := math.Sqrt(sumSq)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / mag)
	}
	return out
}

// cosineSim returns the cosine similarity between two float32 BLOBs.
// Returns 0 for mismatched dims, nil inputs, or zero-magnitude vectors.
func cosineSim(a, b []byte) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) || len(a)%4 != 0 {
		return 0
	}
	n := len(a) / 4

	var dotAB, sumA, sumB float64
	for i := 0; i < n; i++ {
		fa := float64(math.Float32frombits(binary.LittleEndian.Uint32(a[i*4:])))
		fb := float64(math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:])))
		dotAB += fa * fb
		sumA += fa * fa
		sumB += fb * fb
	}
	if sumA == 0 || sumB == 0 {
		return 0
	}
	return dotAB / (math.Sqrt(sumA) * math.Sqrt(sumB))
}
