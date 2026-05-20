// Package web serves a local HTTP dashboard for exploring an archaeologist index.
//
// Why a local web UI alongside the MCP server?
// The MCP tools are designed for AI clients (Claude, Zed, Cursor). The web
// dashboard is for the human developer: a browser-based view of the same data
// that lets you run queries, browse architecture, and render call-graph diagrams
// without needing a connected AI session.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/yoannchl/git-archaeologist/internal/gitlog"
	"github.com/yoannchl/git-archaeologist/internal/llm"
	"github.com/yoannchl/git-archaeologist/internal/retrieve"
	"github.com/yoannchl/git-archaeologist/internal/store"
)

//go:embed static
var staticFiles embed.FS

// Server is the web dashboard HTTP server.
type Server struct {
	store *store.Store
	llm   *llm.Client // may be nil — vector search falls back to FTS
}

// New creates a web Server.
func New(s *store.Store, client *llm.Client) *Server {
	return &Server{store: s, llm: client}
}

// Handler returns an http.Handler that serves the dashboard.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/query", s.apiQuery)
	mux.HandleFunc("/api/entrypoints", s.apiEntrypoints)
	mux.HandleFunc("/api/architecture", s.apiArchitecture)
	mux.HandleFunc("/api/diagram", s.apiDiagram)

	// Serve the embedded static files (index.html, etc.).
	sub, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}

// --- API types ---------------------------------------------------------------

type queryResult struct {
	Qualified string   `json:"qualified"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	File      string   `json:"file"`
	Line      int      `json:"line"`
	Doc       string   `json:"doc"`
	Signature string   `json:"signature"`
	Score     float32  `json:"score"`
	Reasons   []string `json:"reasons"`
}

type architectureResp struct {
	Packages []pkgInfo  `json:"packages"`
	HotFiles []hotFile  `json:"hot_files"`
}
type pkgInfo struct {
	Path    string `json:"path"`
	Symbols int    `json:"symbols"`
}
type hotFile struct {
	Path    string `json:"path"`
	Package string `json:"package"`
	Churn   int    `json:"churn"`
	Commits int    `json:"commits"`
}

type entrypointsResp struct {
	Mains            []symBrief `json:"mains"`
	Inits            []symBrief `json:"inits"`
	HTTPRoutes       []symBrief `json:"http_routes"`
	CobraCommands    []symBrief `json:"cobra_commands"`
	GRPCServices     []symBrief `json:"grpc_services"`
	GoroutineWorkers []symBrief `json:"goroutine_workers"`
	CronJobs         []symBrief `json:"cron_jobs"`
}
type symBrief struct {
	Qualified string `json:"qualified"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Doc       string `json:"doc"`
}

type diagramResp struct {
	Mermaid     string `json:"mermaid"`
	NodeCount   int    `json:"node_count"`
	EdgeCount   int    `json:"edge_count"`
	Description string `json:"description"`
}

// --- API handlers ------------------------------------------------------------

func (s *Server) apiQuery(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		jsonError(w, "missing q parameter", http.StatusBadRequest)
		return
	}
	k := 25
	if kStr := r.URL.Query().Get("k"); kStr != "" {
		if n, err := strconv.Atoi(kStr); err == nil && n > 0 {
			k = n
		}
	}

	opt := retrieve.DefaultOptions()
	opt.MaxResults = k

	hits, err := retrieve.Query(r.Context(), s.store, s.llm, q, opt)
	if err != nil {
		jsonError(w, fmt.Sprintf("query: %v", err), http.StatusInternalServerError)
		return
	}

	out := make([]queryResult, 0, len(hits))
	for _, h := range hits {
		qr := queryResult{
			Qualified: h.Symbol.Qualified,
			Name:      h.Symbol.Name,
			Kind:      h.Symbol.Kind,
			Line:      h.Symbol.LineStart,
			Doc:       truncate(h.Symbol.Doc, 300),
			Signature: h.Symbol.Signature,
			Score:     h.Score,
			Reasons:   h.Reasons,
		}
		if h.File != nil {
			qr.File = h.File.Path
		}
		out = append(out, qr)
	}
	jsonOK(w, out)
}

func (s *Server) apiArchitecture(w http.ResponseWriter, r *http.Request) {
	resp := architectureResp{}

	rows, err := s.store.DB().Query(
		`SELECT qualified, (SELECT COUNT(*) FROM symbols s2 WHERE s2.kind != 'package' AND s2.qualified LIKE s.qualified || '.%') as cnt
		 FROM symbols s WHERE kind = 'package' ORDER BY cnt DESC LIMIT 50`)
	if err == nil {
		for rows.Next() {
			var pi pkgInfo
			_ = rows.Scan(&pi.Path, &pi.Symbols)
			resp.Packages = append(resp.Packages, pi)
		}
		rows.Close()
	}

	if hot, err := gitlog.HotFiles(s.store, 20); err == nil {
		for _, h := range hot {
			resp.HotFiles = append(resp.HotFiles, hotFile{
				Path: h.Path, Package: h.Package, Churn: h.Churn, Commits: h.Commits,
			})
		}
	}

	jsonOK(w, resp)
}

func (s *Server) apiEntrypoints(w http.ResponseWriter, r *http.Request) {
	resp := entrypointsResp{}
	db := s.store.DB()

	queries := []struct {
		sql  string
		args []any
		dest *[]symBrief
	}{
		{`SELECT qualified,name,kind,COALESCE(f.path,''),s.line_start,s.doc FROM symbols s LEFT JOIN files f ON f.id=s.file_id WHERE s.name='main' AND s.kind='func' ORDER BY s.qualified`, nil, &resp.Mains},
		{`SELECT qualified,name,kind,COALESCE(f.path,''),s.line_start,s.doc FROM symbols s LEFT JOIN files f ON f.id=s.file_id WHERE s.name='init' AND s.kind='func' ORDER BY s.qualified`, nil, &resp.Inits},
		{`SELECT s.qualified,s.name,s.kind,COALESCE(f.path,''),s.line_start,s.doc FROM symbols s LEFT JOIN files f ON f.id=s.file_id WHERE s.signature LIKE '%http.ResponseWriter%' OR s.signature LIKE '%http.Handler%' OR s.signature LIKE '%gin.Context%' OR s.signature LIKE '%echo.Context%' ORDER BY s.qualified LIMIT 50`, nil, &resp.HTTPRoutes},
		{`SELECT s.qualified,s.name,s.kind,COALESCE(f.path,''),s.line_start,s.doc FROM symbols s LEFT JOIN files f ON f.id=s.file_id WHERE (s.name LIKE '%Cmd' OR s.name LIKE '%Command' OR s.name='Execute') AND s.kind IN ('var','func') ORDER BY s.qualified LIMIT 30`, nil, &resp.CobraCommands},
		{`SELECT s.qualified,s.name,s.kind,COALESCE(f.path,''),s.line_start,s.doc FROM symbols s LEFT JOIN files f ON f.id=s.file_id WHERE s.name LIKE 'Register%Server' OR s.signature LIKE '%grpc.Server%' ORDER BY s.qualified LIMIT 30`, nil, &resp.GRPCServices},
		{`SELECT DISTINCT s.qualified,s.name,s.kind,COALESCE(f.path,''),s.line_start,s.doc FROM symbols s LEFT JOIN files f ON f.id=s.file_id JOIN edges e ON e.dst=s.id WHERE e.relation='spawns' ORDER BY s.qualified LIMIT 50`, nil, &resp.GoroutineWorkers},
		{`SELECT DISTINCT s.qualified,s.name,s.kind,COALESCE(f.path,''),s.line_start,s.doc FROM symbols s LEFT JOIN files f ON f.id=s.file_id JOIN edges e ON e.dst=s.id WHERE e.relation='schedules' ORDER BY s.qualified LIMIT 30`, nil, &resp.CronJobs},
	}

	for _, q := range queries {
		rows, err := db.Query(q.sql, q.args...)
		if err != nil {
			continue
		}
		for rows.Next() {
			var b symBrief
			_ = rows.Scan(&b.Qualified, &b.Name, &b.Kind, &b.File, &b.Line, &b.Doc)
			b.Doc = truncate(b.Doc, 200)
			*q.dest = append(*q.dest, b)
		}
		rows.Close()
	}

	jsonOK(w, resp)
}

func (s *Server) apiDiagram(w http.ResponseWriter, r *http.Request) {
	symbol := r.URL.Query().Get("symbol")
	kind := r.URL.Query().Get("kind")
	depth := 2
	if d, err := strconv.Atoi(r.URL.Query().Get("depth")); err == nil && d >= 1 && d <= 4 {
		depth = d
	}

	var resp diagramResp
	var err error
	switch {
	case kind == "" || kind == "call_graph":
		resp, err = buildCallGraph(s.store, symbol, depth)
	case kind == "package_deps":
		resp, err = buildPackageDeps(s.store, symbol)
	default:
		jsonError(w, fmt.Sprintf("unknown kind %q", kind), http.StatusBadRequest)
		return
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, resp)
}

// --- diagram builders (mirrors mcpserver/diagram.go logic) -------------------

func buildCallGraph(s *store.Store, qualified string, depth int) (diagramResp, error) {
	if qualified == "" {
		return diagramResp{}, fmt.Errorf("call_graph requires a symbol name")
	}
	db := s.DB()

	var rootID int64
	var rootName, rootKind string
	if err := db.QueryRow(`SELECT id, name, kind FROM symbols WHERE qualified = ? LIMIT 1`, qualified).
		Scan(&rootID, &rootName, &rootKind); err != nil {
		return diagramResp{}, fmt.Errorf("symbol %q not found", qualified)
	}

	type edge struct{ src, dst int64 }
	nodeIDs := map[int64]bool{rootID: true}
	edgeSeen := map[string]bool{}
	var edges []edge

	addEdge := func(src, dst int64) {
		key := fmt.Sprintf("%d>%d", src, dst)
		if !edgeSeen[key] {
			edgeSeen[key] = true
			edges = append(edges, edge{src, dst})
		}
	}

	frontier := []int64{rootID}
	for hop := 0; hop < depth && len(frontier) > 0 && len(nodeIDs) < 50; hop++ {
		ph, args := inPlaceholders(frontier)
		rows, err := db.Query(fmt.Sprintf(`SELECT DISTINCT src, dst FROM edges WHERE src IN (%s) AND relation='calls'`, ph), args...)
		if err != nil {
			break
		}
		var next []int64
		for rows.Next() {
			var src, dst int64
			_ = rows.Scan(&src, &dst)
			addEdge(src, dst)
			if !nodeIDs[dst] && len(nodeIDs) < 50 {
				nodeIDs[dst] = true
				next = append(next, dst)
			}
		}
		rows.Close()
		frontier = next
	}

	rows, _ := db.Query(`SELECT DISTINCT src, dst FROM edges WHERE dst = ? AND relation='calls'`, rootID)
	if rows != nil {
		for rows.Next() {
			var src, dst int64
			_ = rows.Scan(&src, &dst)
			addEdge(src, dst)
			if !nodeIDs[src] && len(nodeIDs) < 50 {
				nodeIDs[src] = true
			}
		}
		rows.Close()
	}

	type nodeInfo struct{ name, kind string }
	labels := make(map[int64]nodeInfo, len(nodeIDs))
	ids := make([]int64, 0, len(nodeIDs))
	for id := range nodeIDs {
		ids = append(ids, id)
	}
	ph, args := inPlaceholders(ids)
	lrows, err := db.Query(fmt.Sprintf(`SELECT id, name, kind FROM symbols WHERE id IN (%s)`, ph), args...)
	if err == nil {
		for lrows.Next() {
			var id int64
			var n, k string
			_ = lrows.Scan(&id, &n, &k)
			labels[id] = nodeInfo{n, k}
		}
		lrows.Close()
	}

	var sb strings.Builder
	sb.WriteString("flowchart LR\n")
	for id, info := range labels {
		lbl := mermaidQuote(info.name)
		if id == rootID {
			fmt.Fprintf(&sb, "    N%d([%s]):::root\n", id, lbl)
		} else {
			fmt.Fprintf(&sb, "    N%d[%s]\n", id, lbl)
		}
	}
	sb.WriteString("    classDef root fill:#f96,stroke:#333,font-weight:bold\n")
	for _, e := range edges {
		fmt.Fprintf(&sb, "    N%d --> N%d\n", e.src, e.dst)
	}

	return diagramResp{
		Mermaid:   sb.String(),
		NodeCount: len(nodeIDs),
		EdgeCount: len(edges),
		Description: fmt.Sprintf("Call graph around %s (%s), depth %d — %d nodes, %d edges",
			rootName, rootKind, depth, len(nodeIDs), len(edges)),
	}, nil
}

func buildPackageDeps(s *store.Store, focus string) (diagramResp, error) {
	rows, err := s.DB().Query(`
		SELECT DISTINCT f1.package, f2.package
		FROM edges e
		JOIN symbols s1 ON s1.id = e.src
		JOIN symbols s2 ON s2.id = e.dst
		JOIN files   f1 ON f1.id = s1.file_id
		JOIN files   f2 ON f2.id = s2.file_id
		WHERE e.relation = 'calls' AND f1.package != f2.package
		  AND s1.file_id != 0 AND s2.file_id != 0
		LIMIT 300`)
	if err != nil {
		return diagramResp{}, fmt.Errorf("query package edges: %w", err)
	}
	defer rows.Close()

	type pair struct{ src, dst string }
	pkgSet := map[string]bool{}
	seen := map[string]bool{}
	var edges []pair
	for rows.Next() {
		var src, dst string
		_ = rows.Scan(&src, &dst)
		if focus != "" && src != focus && dst != focus {
			continue
		}
		key := src + "→" + dst
		if !seen[key] {
			seen[key] = true
			edges = append(edges, pair{src, dst})
			pkgSet[src] = true
			pkgSet[dst] = true
		}
	}

	pkgIdx := make(map[string]int, len(pkgSet))
	i := 0
	for p := range pkgSet {
		pkgIdx[p] = i
		i++
	}

	var sb strings.Builder
	sb.WriteString("flowchart LR\n")
	for pkg, idx := range pkgIdx {
		lbl := mermaidQuote(pkgShortName(pkg))
		if pkg == focus {
			fmt.Fprintf(&sb, "    P%d([%s]):::root\n", idx, lbl)
		} else {
			fmt.Fprintf(&sb, "    P%d[%s]\n", idx, lbl)
		}
	}
	if focus != "" {
		sb.WriteString("    classDef root fill:#f96,stroke:#333,font-weight:bold\n")
	}
	for _, e := range edges {
		fmt.Fprintf(&sb, "    P%d --> P%d\n", pkgIdx[e.src], pkgIdx[e.dst])
	}

	desc := "Package dependency graph"
	if focus != "" {
		desc += " (focused on " + focus + ")"
	}
	desc += fmt.Sprintf(": %d packages, %d cross-package call edges", len(pkgSet), len(edges))

	return diagramResp{
		Mermaid:   sb.String(),
		NodeCount: len(pkgSet),
		EdgeCount: len(edges),
		Description: desc,
	}, nil
}

// --- helpers -----------------------------------------------------------------

func inPlaceholders(ids []int64) (string, []any) {
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	return strings.Join(ph, ","), args
}

func mermaidQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `#quot;`) + `"`
}

func pkgShortName(pkg string) string {
	parts := strings.Split(pkg, "/")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

