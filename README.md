# Git Archaeologist

> Understand legacy Go repositories. Locally. In minutes.

Git Archaeologist indexes a Go repository's **code graph**, **vector embeddings**, and **git history**, then exposes that index as an **MCP server** so any MCP-aware client (Claude Desktop, Zed, Cursor, etc.) can answer questions like:

- *Where is the payment system handled?*
- *Which files do I touch to add a new auth provider?*
- *Why does this route call Redis?*
- *What are the entrypoints of this binary?*

It runs entirely on-prem against a local LLM (Ollama). No code leaves your machine.

---

## Why this exists

LLMs are good at reading 200 lines of code. They are bad at understanding a 200k-line repo, because:

- **RAG over chunks loses structure.** "Where is the payment system?" doesn't match `ChargeCustomer()` semantically.
- **Naïve text search misses semantics.** Searching for "payment" misses files named `charge.go`.
- **Neither knows the call graph.** Once you find `ChargeCustomer`, you need its callers (the HTTP handler) and callees (the Stripe client) for the answer to be useful.

Git Archaeologist combines all three signals: **vector + lexical + graph**. The retrieval pipeline is the secret sauce — the LLM is just the renderer.

---

## How it works

```
┌─────────────────────────────────────────────────┐
│  MCP Server (stdio)                              │
│  query · find_entrypoints · explain_symbol       │
│  where_to_add · architecture_overview            │
└─────────────────────────────────────────────────┘
                       ▼
┌─────────────────────────────────────────────────┐
│  Hybrid Retrieval                                │
│  vector(Ollama) ⊕ FTS5 ⊕ graph expansion         │
└─────────────────────────────────────────────────┘
        ▼              ▼              ▼
┌──────────────┐ ┌──────────────┐ ┌──────────────┐
│ Code Graph   │ │ Embeddings   │ │ Git History  │
│ (SQLite)     │ │ (SQLite)     │ │ (SQLite)     │
└──────────────┘ └──────────────┘ └──────────────┘
        ▲              ▲              ▲
        └──────────────┴──────────────┘
                       │
┌─────────────────────────────────────────────────┐
│  Indexer                                         │
│  go/packages + go/types + go-git + Ollama        │
└─────────────────────────────────────────────────┘
```

A single SQLite file (`.archaeo/index.db`) holds:

- **`files`, `symbols`, `edges`** — the typed code graph (`calls`, `implements`, `contains`).
- **`symbols_fts`** — FTS5 lexical index over names + docs + signatures.
- **`embeddings`** — vector per symbol (BLOB of float32, brute-force cosine).
- **`commits`, `file_commits`** — git history for hot-file detection.

The parser uses `go/packages` + `go/types`, **not** tree-sitter, because we need real type resolution to compute the call graph and interface satisfaction.

---

## Install

Prerequisites:

- Go 1.25+ (required by the MCP SDK v1.4.x)
- [Ollama](https://ollama.com) running locally
- A chat model (`ollama pull qwen2.5-coder:14b`)
- An embedding model (`ollama pull nomic-embed-text`)

```bash
git clone https://github.com/yourname/git-archaeologist
cd git-archaeologist
make install   # installs `archaeo` and `archaeo-mcp` to $GOBIN
```

---

## Usage

### 1. Index a repo

```bash
cd /path/to/your/legacy/go/project
archaeo index
```

This creates `.archaeo/index.db` (gitignore it). On a 100k-LOC Go repo, expect ~30s without embeddings, ~5 min with.

Flags worth knowing:

| Flag | Default | Notes |
|---|---|---|
| `--repo` | `.` | Path to the repo root |
| `--no-embed` | `false` | Skip embeddings (retrieval falls back to FTS+graph) |
| `--no-git` | `false` | Skip git history |
| `--ollama` | `http://127.0.0.1:11434` | Ollama base URL |
| `--embed-model` | `nomic-embed-text` | Override the embedding model |
| `--max-commits` | `5000` | Cap git traversal |

### 2. Quick ad-hoc query (debug CLI)

```bash
archaeo query "where is payment handled"
```

This bypasses MCP and prints top hits with their scores and provenance.

### 3. Connect via MCP

Add this to your MCP client config (Claude Desktop, Zed, etc.):

```json
{
  "mcpServers": {
    "git-archaeologist": {
      "command": "archaeo-mcp",
      "args": ["--repo", "/absolute/path/to/your/repo"]
    }
  }
}
```

Then ask things like *"use the git-archaeologist to find where the auth middleware lives"*.

---

## Tools exposed by the MCP server

| Tool | Purpose |
|---|---|
| `query` | Natural-language question → ranked symbols. The default entry point. |
| `find_entrypoints` | `main()`, `init()`, HTTP route handlers. Always start here when onboarding. |
| `explain_symbol` | Given a qualified name, return signature + doc + callers + callees + implementations. |
| `where_to_add` | Given a feature description, suggest modification sites. |
| `architecture_overview` | Package layout + hottest files by git churn. |

---

## Design choices worth knowing

- **One SQLite file per repo.** No external infra. Brute-force cosine is fine up to ~100k embedded symbols on a laptop. We can swap in `sqlite-vec` or Qdrant if/when we hit a wall.
- **Embed funcs + types + files, not packages or vars.** Granularity sweet spot for retrieval quality vs. index time.
- **Composite embed text** = `kind + qualified + signature + doc + first ~80 LOC of body`. Doc gives intent, signature gives shape, code prefix gives structure.
- **Graph expansion follows callers, not callees,** by default. Onboarding questions ("where is X handled?") want the caller (the handler), not the callee (the library).
- **Hot files = sum(churn) over recorded history.** Joined with LOC in the overview tool — high LOC × high churn is the textbook "scary file".
- **Generated and test files are de-prioritised, not hidden.** They're sometimes the only place a pattern is documented.

---

## Roadmap

- [ ] Incremental re-index on file change
- [ ] Mermaid diagram generation (call graph, sequence per endpoint)
- [ ] `sqlite-vec` extension auto-loading for >100k symbol repos
- [ ] Multi-language via tree-sitter fallback (Python first)
- [ ] Web dashboard for non-MCP browsing
- [ ] Fine-tuned re-ranker for the top-50 → top-15 squeeze

---

## Project layout

```
cmd/
  archaeo/          CLI for indexing + ad-hoc queries
  archaeo-mcp/      MCP server (stdio transport)
internal/
  store/            SQLite schema, typed helpers, embedding storage
  parser/           go/packages → symbols + edges
  gitlog/           go-git → commits + churn
  llm/              Ollama client (chat + embeddings)
  embed/            embedding pipeline (select + compose + persist)
  retrieve/         hybrid retrieval (vector + FTS + graph)
  index/            orchestrator: parse → git → embed
  mcpserver/        the 5 MCP tools
testdata/sample/    tiny sample Go repo used by parser_test
```
