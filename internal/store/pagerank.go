// pagerank.go computes a simple iterative PageRank over the call+import graph
// and writes normalised scores [0, 1] back into the symbols table.
//
// Why PageRank?
// FTS and vector similarity find symbols that *match* a query, but they can't
// distinguish a utility function called by 50 handlers from a one-off helper
// called once. PageRank surfaces the former — the code that *matters* to the
// architecture — and gives it a small retrieval boost.
//
// Edge semantics: src→dst means "src calls/imports dst". dst therefore receives
// rank contributions from its callers. A widely-called function converges to a
// high rank, which is exactly what we want for onboarding questions like "where
// is the payment logic?".
package store

const pagerankDamping = 0.85
const pagerankIters = 20

// ComputePageRank runs iterative PageRank over calls+imports edges and persists
// normalised scores to symbols.pagerank. Non-fatal: a repo with no edges simply
// leaves all scores at 0.
func (s *Store) ComputePageRank() error {
	// --- 1. Load all symbol IDs -------------------------------------------------
	idRows, err := s.db.Query(`SELECT id FROM symbols`)
	if err != nil {
		return err
	}
	var ids []int64
	for idRows.Next() {
		var id int64
		if err := idRows.Scan(&id); err != nil {
			idRows.Close()
			return err
		}
		ids = append(ids, id)
	}
	idRows.Close()
	if err := idRows.Err(); err != nil {
		return err
	}
	n := len(ids)
	if n == 0 {
		return nil
	}

	// --- 2. Load directed edges (calls + imports) --------------------------------
	eRows, err := s.db.Query(
		`SELECT src, dst FROM edges WHERE relation IN ('calls','imports')`)
	if err != nil {
		return err
	}
	inNeighbors := make(map[int64][]int64, n) // dst → []src
	outDegree := make(map[int64]int, n)        // src → count
	for eRows.Next() {
		var src, dst int64
		if err := eRows.Scan(&src, &dst); err != nil {
			eRows.Close()
			return err
		}
		inNeighbors[dst] = append(inNeighbors[dst], src)
		outDegree[src]++
	}
	eRows.Close()
	if err := eRows.Err(); err != nil {
		return err
	}

	// --- 3. Iterative PageRank --------------------------------------------------
	initial := 1.0 / float64(n)
	rank := make(map[int64]float64, n)
	for _, id := range ids {
		rank[id] = initial
	}

	base := (1 - pagerankDamping) / float64(n)
	for range pagerankIters {
		next := make(map[int64]float64, n)
		for _, id := range ids {
			next[id] = base
		}
		for dst, srcs := range inNeighbors {
			var contrib float64
			for _, src := range srcs {
				if od := outDegree[src]; od > 0 {
					contrib += rank[src] / float64(od)
				}
			}
			next[dst] += pagerankDamping * contrib
		}
		rank = next
	}

	// --- 4. Normalise to [0, 1] -------------------------------------------------
	var maxR float64
	for _, r := range rank {
		if r > maxR {
			maxR = r
		}
	}
	if maxR > 0 {
		for id := range rank {
			rank[id] /= maxR
		}
	}

	// --- 5. Persist -------------------------------------------------------------
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`UPDATE symbols SET pagerank = ? WHERE id = ?`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.Exec(rank[id], id); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
