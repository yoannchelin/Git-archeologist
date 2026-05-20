package store

// Schema defines the complete database schema for a Git Archaeologist index.
//
// Design notes:
//   - One SQLite file per repo, stored at <repo>/.archaeo/index.db
//   - `symbols` is the central table: every package, file, type, func, method
//   - `edges` stores typed relations (calls, implements, uses, contains)
//   - `embeddings` holds vectors as BLOB (raw float32 little-endian);
//     we use sqlite-vec if available, otherwise brute-force cosine in Go.
//   - `symbols_fts` is a contentless FTS5 mirror for lexical search.
//   - `commits` / `file_commits` give us churn, recency, blame.
//
// We intentionally keep the schema small. Anything that can be recomputed
// from these tables (call depth, centrality, hotness score) lives in code.
const Schema = `
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS files (
    id          INTEGER PRIMARY KEY,
    path        TEXT NOT NULL UNIQUE,  -- relative to repo root
    package     TEXT NOT NULL,
    loc         INTEGER NOT NULL DEFAULT 0,
    is_test     INTEGER NOT NULL DEFAULT 0,
    is_generated INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_files_package ON files(package);

-- A symbol is any named, addressable entity: package, file, type, func, method, var, const.
CREATE TABLE IF NOT EXISTS symbols (
    id          INTEGER PRIMARY KEY,
    kind        TEXT NOT NULL,         -- 'package','file','type','func','method','var','const','interface'
    name        TEXT NOT NULL,         -- short name, e.g. "ChargeCustomer"
    qualified   TEXT NOT NULL UNIQUE,  -- e.g. "github.com/x/y/payment.ChargeCustomer"
    file_id     INTEGER REFERENCES files(id),
    line_start  INTEGER NOT NULL DEFAULT 0,
    line_end    INTEGER NOT NULL DEFAULT 0,
    signature   TEXT NOT NULL DEFAULT '',
    doc         TEXT NOT NULL DEFAULT '',
    exported    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_symbols_name ON symbols(name);
CREATE INDEX IF NOT EXISTS idx_symbols_kind ON symbols(kind);
CREATE INDEX IF NOT EXISTS idx_symbols_file ON symbols(file_id);

-- Typed graph edges between symbols.
-- relation: 'calls','implements','uses','contains','satisfies','imports'
CREATE TABLE IF NOT EXISTS edges (
    src         INTEGER NOT NULL REFERENCES symbols(id),
    dst         INTEGER NOT NULL REFERENCES symbols(id),
    relation    TEXT NOT NULL,
    weight      REAL NOT NULL DEFAULT 1.0,
    PRIMARY KEY (src, dst, relation)
);
CREATE INDEX IF NOT EXISTS idx_edges_dst ON edges(dst, relation);
CREATE INDEX IF NOT EXISTS idx_edges_src ON edges(src, relation);

-- Embeddings: one row per symbol that we embedded (typically funcs + types + files).
-- Vector stored as BLOB of len*4 bytes (float32 LE). Dim is kept per-row to
-- detect mismatches if the user swaps embedding models.
CREATE TABLE IF NOT EXISTS embeddings (
    symbol_id   INTEGER PRIMARY KEY REFERENCES symbols(id) ON DELETE CASCADE,
    dim         INTEGER NOT NULL,
    vec         BLOB NOT NULL,
    model       TEXT NOT NULL
);

-- HNSW vector index via sqlite-vec. Stores unit-normalised float32[768] vectors
-- (nomic-embed-text default) so that L2 distance ≡ cosine distance. Rowid maps
-- 1-to-1 with symbols.id / embeddings.symbol_id. Created here; populated by
-- PutEmbedding. Queried by NearestNeighbors when hasHNSW=true.
-- Re-index required when switching embedding models with different dimensions.
CREATE VIRTUAL TABLE IF NOT EXISTS vec_embeddings USING vec0(
    embedding float[768]
);

-- FTS5 lexical index on symbol name + doc + signature.
-- Contentless: we look up the row in symbols by rowid.
CREATE VIRTUAL TABLE IF NOT EXISTS symbols_fts USING fts5(
    name, qualified, doc, signature,
    content='symbols', content_rowid='id',
    tokenize = 'porter unicode61 remove_diacritics 2'
);

CREATE TRIGGER IF NOT EXISTS symbols_ai AFTER INSERT ON symbols BEGIN
    INSERT INTO symbols_fts(rowid, name, qualified, doc, signature)
    VALUES (new.id, new.name, new.qualified, new.doc, new.signature);
END;
CREATE TRIGGER IF NOT EXISTS symbols_ad AFTER DELETE ON symbols BEGIN
    INSERT INTO symbols_fts(symbols_fts, rowid, name, qualified, doc, signature)
    VALUES('delete', old.id, old.name, old.qualified, old.doc, old.signature);
END;
CREATE TRIGGER IF NOT EXISTS symbols_au AFTER UPDATE ON symbols BEGIN
    INSERT INTO symbols_fts(symbols_fts, rowid, name, qualified, doc, signature)
    VALUES('delete', old.id, old.name, old.qualified, old.doc, old.signature);
    INSERT INTO symbols_fts(rowid, name, qualified, doc, signature)
    VALUES (new.id, new.name, new.qualified, new.doc, new.signature);
END;

-- Git layer.
CREATE TABLE IF NOT EXISTS commits (
    hash        TEXT PRIMARY KEY,
    author      TEXT NOT NULL,
    email       TEXT NOT NULL,
    ts          INTEGER NOT NULL,    -- unix seconds
    subject     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS file_commits (
    file_id     INTEGER NOT NULL REFERENCES files(id),
    commit_hash TEXT NOT NULL REFERENCES commits(hash),
    added       INTEGER NOT NULL DEFAULT 0,
    deleted     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (file_id, commit_hash)
);
CREATE INDEX IF NOT EXISTS idx_fc_file ON file_commits(file_id);
`
