// Package store wraps the SQLite database used by the archaeologist.
//
// All persistence — code graph, embeddings, FTS, git history — lives here.
// The package deliberately exposes typed methods rather than letting callers
// write SQL, so the schema can evolve without breaking the rest of the code.
package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

)

// vecEmbeddingDim is the fixed dimension expected by the vec_embeddings HNSW
// table (matches nomic-embed-text output). Vectors with a different dimension
// are silently skipped from the HNSW index and handled by cosine_sim fallback.
const vecEmbeddingDim = 768

// Store is a handle to a single repo's index database.
type Store struct {
	db      *sql.DB
	path    string
	hasHNSW bool // true once vec_embeddings contains at least one vector
}

// Symbol mirrors the `symbols` table.
type Symbol struct {
	ID        int64
	Kind      string
	Name      string
	Qualified string
	FileID    int64
	LineStart int
	LineEnd   int
	Signature string
	Doc       string
	Exported  bool
	PageRank  float64 // normalised [0,1]; 0 = not yet computed or isolated node
}

// File mirrors the `files` table.
type File struct {
	ID          int64
	Path        string
	Package     string
	LOC         int
	IsTest      bool
	IsGenerated bool
	Language    string // "go", "typescript", etc. — defaults to "go"
}

// Edge is a typed relation between two symbols.
type Edge struct {
	Src      int64
	Dst      int64
	Relation string
	Weight   float64
}

// Open opens (and creates if needed) the index DB for a repo.
//
// The DB lives at <repoRoot>/.archaeo/index.db. The directory is created
// on demand. The schema is applied idempotently on every open.
func Open(repoRoot string) (*Store, error) {
	dir := filepath.Join(repoRoot, ".archaeo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	dbPath := filepath.Join(dir, "index.db")
	// _busy_timeout: SQLite will retry locked writes for up to 5s, which
	// makes the indexer's parallel inserts robust without us managing locks.
	dsn := fmt.Sprintf("file:%s?_busy_timeout=5000&_journal_mode=WAL", dbPath)
	db, err := sql.Open("sqlite3_archaeo", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(Schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := applyMigrations(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db, path: dbPath}
	// Detect whether the HNSW index is already populated (existing index opened
	// after a previous embed run). A quick COUNT is cheaper than a full scan.
	var vecCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM vec_embeddings`).Scan(&vecCount); err == nil {
		s.hasHNSW = vecCount > 0
	}
	return s, nil
}

// applyMigrations runs schema additions that post-date the initial Schema const.
// Each ALTER is idempotent: "duplicate column name" is silently ignored.
func applyMigrations(db *sql.DB) error {
	// S2: multi-language support requires a language column on files.
	if _, err := db.Exec(`ALTER TABLE files ADD COLUMN language TEXT NOT NULL DEFAULT 'go'`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate files.language: %w", err)
		}
	}
	// PageRank scores, computed after each index build.
	if _, err := db.Exec(`ALTER TABLE symbols ADD COLUMN pagerank REAL NOT NULL DEFAULT 0`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migrate symbols.pagerank: %w", err)
		}
	}
	return nil
}

// Close releases the DB handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw *sql.DB for callers that need ad-hoc queries.
// Use sparingly — prefer adding a typed method here.
func (s *Store) DB() *sql.DB { return s.db }

// Path is the on-disk path of the index file.
func (s *Store) Path() string { return s.path }

// SetMeta writes a key/value pair into the meta table (used for things like
// last-indexed commit, embedding model name, schema version).
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

// GetMeta returns a meta value, or ("", false) if not set.
func (s *Store) GetMeta(key string) (string, bool, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// --- Batch insertion helpers -------------------------------------------------

// BatchInsert is a small builder that accumulates work for one transaction.
// Use it during indexing to amortise commit cost across thousands of rows.
type BatchInsert struct {
	tx       *sql.Tx
	fileStmt *sql.Stmt
	symStmt  *sql.Stmt
	edgeStmt *sql.Stmt
}

// Begin starts a write transaction and prepares the hot-path statements.
func (s *Store) Begin() (*BatchInsert, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	fileStmt, err := tx.Prepare(`
		INSERT INTO files(path, package, loc, is_test, is_generated, language)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			package = excluded.package,
			loc = excluded.loc,
			is_test = excluded.is_test,
			is_generated = excluded.is_generated,
			language = excluded.language
		RETURNING id`)
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("prepare files: %w", err)
	}
	symStmt, err := tx.Prepare(`
		INSERT INTO symbols(kind, name, qualified, file_id, line_start, line_end, signature, doc, exported)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(qualified) DO UPDATE SET
			kind = excluded.kind,
			name = excluded.name,
			file_id = excluded.file_id,
			line_start = excluded.line_start,
			line_end = excluded.line_end,
			signature = excluded.signature,
			doc = excluded.doc,
			exported = excluded.exported
		RETURNING id`)
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("prepare symbols: %w", err)
	}
	edgeStmt, err := tx.Prepare(`
		INSERT INTO edges(src, dst, relation, weight) VALUES(?, ?, ?, ?)
		ON CONFLICT(src, dst, relation) DO UPDATE SET weight = excluded.weight`)
	if err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("prepare edges: %w", err)
	}
	return &BatchInsert{tx: tx, fileStmt: fileStmt, symStmt: symStmt, edgeStmt: edgeStmt}, nil
}

// PutFile inserts/updates a file row and returns its id.
func (b *BatchInsert) PutFile(f File) (int64, error) {
	lang := f.Language
	if lang == "" {
		lang = "go"
	}
	var id int64
	err := b.fileStmt.QueryRow(
		f.Path, f.Package, f.LOC, boolToInt(f.IsTest), boolToInt(f.IsGenerated), lang,
	).Scan(&id)
	return id, err
}

// PutSymbol inserts/updates a symbol row and returns its id.
func (b *BatchInsert) PutSymbol(s Symbol) (int64, error) {
	var id int64
	err := b.symStmt.QueryRow(
		s.Kind, s.Name, s.Qualified, nullableInt64(s.FileID),
		s.LineStart, s.LineEnd, s.Signature, s.Doc, boolToInt(s.Exported),
	).Scan(&id)
	return id, err
}

// PutEdge inserts/updates a typed edge.
func (b *BatchInsert) PutEdge(e Edge) error {
	_, err := b.edgeStmt.Exec(e.Src, e.Dst, e.Relation, e.Weight)
	return err
}

// Commit finalises the batch.
func (b *BatchInsert) Commit() error {
	defer func() {
		_ = b.fileStmt.Close()
		_ = b.symStmt.Close()
		_ = b.edgeStmt.Close()
	}()
	return b.tx.Commit()
}

// Rollback aborts the batch.
func (b *BatchInsert) Rollback() error {
	defer func() {
		_ = b.fileStmt.Close()
		_ = b.symStmt.Close()
		_ = b.edgeStmt.Close()
	}()
	return b.tx.Rollback()
}

// --- Incremental update ------------------------------------------------------

// DeletePackageData removes all symbols and edges touching a Go package.
// Both src and dst edges are deleted before symbols to satisfy FK constraints.
// Embeddings cascade via the FK ON DELETE CASCADE; FTS via the AFTER DELETE trigger.
func (s *Store) DeletePackageData(pkgPath string) error {
	// Delete any edge that references a symbol in this package, whether as
	// caller (src) or callee (dst). Must come before symbol deletion.
	if _, err := s.db.Exec(`
		DELETE FROM edges WHERE src IN (
			SELECT sym.id FROM symbols sym
			JOIN files f ON f.id = sym.file_id
			WHERE f.package = ?
		) OR dst IN (
			SELECT sym.id FROM symbols sym
			JOIN files f ON f.id = sym.file_id
			WHERE f.package = ?
		)`, pkgPath, pkgPath); err != nil {
		return fmt.Errorf("delete edges: %w", err)
	}
	if _, err := s.db.Exec(`
		DELETE FROM symbols WHERE file_id IN (
			SELECT id FROM files WHERE package = ?
		)`, pkgPath); err != nil {
		return fmt.Errorf("delete symbols: %w", err)
	}
	return nil
}

// --- Read helpers ------------------------------------------------------------

// GetSymbolByID fetches one symbol or returns (nil, nil) if not found.
func (s *Store) GetSymbolByID(id int64) (*Symbol, error) {
	row := s.db.QueryRow(`
		SELECT id, kind, name, qualified, COALESCE(file_id, 0),
		       line_start, line_end, signature, doc, exported, pagerank
		FROM symbols WHERE id = ?`, id)
	return scanSymbol(row)
}

// GetFileByID fetches one file row.
func (s *Store) GetFileByID(id int64) (*File, error) {
	row := s.db.QueryRow(`
		SELECT id, path, package, loc, is_test, is_generated, COALESCE(language, 'go')
		FROM files WHERE id = ?`, id)
	var f File
	var isTest, isGen int
	if err := row.Scan(&f.ID, &f.Path, &f.Package, &f.LOC, &isTest, &isGen, &f.Language); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	f.IsTest = isTest != 0
	f.IsGenerated = isGen != 0
	return &f, nil
}

// RecentFileIDs returns the set of file IDs that received a commit within the
// last `days` days. Used by the retrieval layer to apply a freshness score boost.
func (s *Store) RecentFileIDs(days int) (map[int64]bool, error) {
	cutoff := time.Now().AddDate(0, 0, -days).Unix()
	rows, err := s.db.Query(`
		SELECT DISTINCT fc.file_id
		FROM file_commits fc
		JOIN commits c ON c.hash = fc.commit_hash
		WHERE c.ts >= ?`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]bool)
	for rows.Next() {
		var fid int64
		if err := rows.Scan(&fid); err != nil {
			return nil, err
		}
		out[fid] = true
	}
	return out, rows.Err()
}

// SearchFTS runs a lexical search over symbols. `query` follows FTS5 syntax.
// Returns up to `limit` symbols ordered by bm25.
func (s *Store) SearchFTS(query string, limit int) ([]Symbol, error) {
	rows, err := s.db.Query(`
		SELECT s.id, s.kind, s.name, s.qualified, COALESCE(s.file_id, 0),
		       s.line_start, s.line_end, s.signature, s.doc, s.exported, s.pagerank
		FROM symbols_fts f
		JOIN symbols s ON s.id = f.rowid
		WHERE symbols_fts MATCH ?
		ORDER BY bm25(symbols_fts)
		LIMIT ?`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Symbol
	for rows.Next() {
		sym, err := scanSymbol(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sym)
	}
	return out, rows.Err()
}

// Neighbors returns symbols reachable from `id` via the given relation,
// up to `depth` hops. Useful for graph expansion during retrieval.
//
// `outgoing` controls direction: true follows edges.src=id, false follows edges.dst=id.
func (s *Store) Neighbors(id int64, relation string, depth int, outgoing bool) ([]int64, error) {
	if depth <= 0 {
		return nil, nil
	}
	visited := map[int64]bool{id: true}
	frontier := []int64{id}
	for range depth {
		if len(frontier) == 0 {
			break
		}
		// Build a placeholder list for IN (...). Capped at 500 to avoid
		// runaway expansion on very dense nodes.
		if len(frontier) > 500 {
			frontier = frontier[:500]
		}
		args := make([]any, 0, len(frontier)+1)
		placeholders := ""
		for i, n := range frontier {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, n)
		}
		args = append(args, relation)
		var q string
		if outgoing {
			q = fmt.Sprintf(`SELECT DISTINCT dst FROM edges WHERE src IN (%s) AND relation = ?`, placeholders)
		} else {
			q = fmt.Sprintf(`SELECT DISTINCT src FROM edges WHERE dst IN (%s) AND relation = ?`, placeholders)
		}
		rows, err := s.db.Query(q, args...)
		if err != nil {
			return nil, err
		}
		next := frontier[:0]
		for rows.Next() {
			var n int64
			if err := rows.Scan(&n); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if !visited[n] {
				visited[n] = true
				next = append(next, n)
			}
		}
		_ = rows.Close()
		frontier = append([]int64(nil), next...)
	}
	delete(visited, id)
	out := make([]int64, 0, len(visited))
	for n := range visited {
		out = append(out, n)
	}
	return out, nil
}

// --- Embeddings --------------------------------------------------------------

// PutEmbedding upserts a vector for a symbol in both the canonical embeddings
// table and, when the dimension matches vecEmbeddingDim, the HNSW vec0 index.
// The vec0 insert stores a unit-normalised copy so L2 distance ≡ cosine distance.
func (s *Store) PutEmbedding(symbolID int64, vec []float32, model string) error {
	blob := encodeFloat32(vec)
	if _, err := s.db.Exec(`
		INSERT INTO embeddings(symbol_id, dim, vec, model)
		VALUES(?, ?, ?, ?)
		ON CONFLICT(symbol_id) DO UPDATE SET
			dim = excluded.dim, vec = excluded.vec, model = excluded.model`,
		symbolID, len(vec), blob, model); err != nil {
		return err
	}
	// Mirror into the HNSW index when the dimension matches the vec0 schema.
	// Silently skip (don't fail) if the user is using a non-standard model dim.
	if len(vec) == vecEmbeddingDim {
		normBlob := encodeFloat32(normalizeVec(vec))
		if _, err := s.db.Exec(
			`INSERT OR REPLACE INTO vec_embeddings(rowid, embedding) VALUES (?, ?)`,
			symbolID, normBlob,
		); err == nil {
			s.hasHNSW = true
		}
	}
	return nil
}

// NearestNeighbors returns the k nearest embeddings to query using cosine
// similarity.
//
// When the HNSW index is populated (hasHNSW=true) and the query dimension
// matches vecEmbeddingDim, it delegates to the sqlite-vec vec0 virtual table
// for O(log n) approximate nearest-neighbor search. Otherwise it falls back to
// the O(n) cosine_sim brute-force scan — correct for any dimension and for
// existing indexes that predate the HNSW table.
func (s *Store) NearestNeighbors(query []float32, k int) ([]NeighborHit, error) {
	if s.hasHNSW && len(query) == vecEmbeddingDim {
		return s.nearestNeighborsHNSW(query, k)
	}
	return s.nearestNeighborsCosine(query, k)
}

// nearestNeighborsHNSW queries the vec0 HNSW index. Vectors are stored
// unit-normalised so L2 distance equals cosine distance; we convert back:
// for unit vectors, cosine_sim = 1 − L2²/2.
func (s *Store) nearestNeighborsHNSW(query []float32, k int) ([]NeighborHit, error) {
	normBlob := encodeFloat32(normalizeVec(query))
	rows, err := s.db.Query(
		`SELECT rowid, distance FROM vec_embeddings
		 WHERE embedding MATCH ?
		 ORDER BY distance LIMIT ?`,
		normBlob, k)
	if err != nil {
		// vec0 may error on schema mismatch; fall back rather than propagating.
		return s.nearestNeighborsCosine(query, k)
	}
	defer rows.Close()
	hits := make([]NeighborHit, 0, k)
	for rows.Next() {
		var symbolID int64
		var dist float64
		if err := rows.Scan(&symbolID, &dist); err != nil {
			return nil, err
		}
		// Convert L2 distance on unit vectors to cosine similarity.
		cosine := 1.0 - (dist*dist)/2.0
		hits = append(hits, NeighborHit{SymbolID: symbolID, Score: float32(cosine)})
	}
	return hits, rows.Err()
}

// nearestNeighborsCosine is the O(n) brute-force fallback using cosine_sim().
func (s *Store) nearestNeighborsCosine(query []float32, k int) ([]NeighborHit, error) {
	qBlob := encodeFloat32(query)
	rows, err := s.db.Query(
		`SELECT symbol_id, CAST(cosine_sim(vec, ?) AS REAL) AS score
		 FROM embeddings
		 ORDER BY score DESC
		 LIMIT ?`,
		qBlob, k)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hits := make([]NeighborHit, 0, k)
	for rows.Next() {
		var h NeighborHit
		var score float64
		if err := rows.Scan(&h.SymbolID, &score); err != nil {
			return nil, err
		}
		h.Score = float32(score)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// NeighborHit is a single nearest-neighbor result.
type NeighborHit struct {
	SymbolID int64
	Score    float32
}

// --- internal helpers --------------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSymbol(r rowScanner) (*Symbol, error) {
	var s Symbol
	var exported int
	if err := r.Scan(&s.ID, &s.Kind, &s.Name, &s.Qualified, &s.FileID,
		&s.LineStart, &s.LineEnd, &s.Signature, &s.Doc, &exported, &s.PageRank); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	s.Exported = exported != 0
	return &s, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullableInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func encodeFloat32(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(x))
	}
	return out
}

