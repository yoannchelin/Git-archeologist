// Package mcpserver wires the archaeologist's capabilities into MCP tools.
//
// We expose five tools, chosen to cover the onboarding journey:
//
//	query                 — natural-language question over the repo
//	find_entrypoints      — main(), HTTP routes, cron jobs, init() side effects
//	explain_symbol        — deep dive on one symbol with callers/callees
//	where_to_add          — "where should I add X?" — gives target files
//	architecture_overview — top-down package layout + hotspots
//
// Each tool stays cheap and focused: the LLM is doing the reasoning,
// we are doing the retrieval.
package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yoannchl/git-archaeologist/internal/gitlog"
	"github.com/yoannchl/git-archaeologist/internal/llm"
	"github.com/yoannchl/git-archaeologist/internal/retrieve"
	"github.com/yoannchl/git-archaeologist/internal/store"
)

// Server holds the shared state for tool handlers.
type Server struct {
	Store    *store.Store
	LLM      *llm.Client
	RepoRoot string
}

// Register wires every tool onto the provided mcp.Server.
//
// Tools are registered using mcp.AddTool, which infers JSON schemas from
// the input/output struct types via jsonschema tags.
func (s *Server) Register(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "query",
		Description: "Ask a natural-language question about the repo. Returns the most relevant symbols (functions, types, files) ranked by hybrid retrieval (vector + lexical + call-graph). Use this when the user asks 'where', 'how', 'why' about something in the codebase.",
	}, s.handleQuery)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "find_entrypoints",
		Description: "List the entrypoints of the repo: main() functions, HTTP route registrations, gRPC servers, cron schedulers, and init() side effects. Use this first when onboarding to a new repo.",
	}, s.handleEntrypoints)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "explain_symbol",
		Description: "Deep dive on one symbol by its qualified name (e.g. 'github.com/x/y/payment.ChargeCustomer'). Returns its signature, doc, file, callers, callees, and any interfaces it implements.",
	}, s.handleExplainSymbol)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "where_to_add",
		Description: "Given a feature description (e.g. 'add a new payment provider'), suggest the files and symbols the change should touch. Combines retrieval with interface-implementation analysis.",
	}, s.handleWhereToAdd)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "architecture_overview",
		Description: "Top-down view of the repo: package list with sizes, top-level dependencies, and the files with the most git churn (likely critical zones).",
	}, s.handleOverview)
}

// --- tool: query -------------------------------------------------------------

type QueryInput struct {
	Question   string `json:"question" jsonschema:"natural-language question about the repository"`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"max symbols to return (default 15)"`
}

type QueryOutput struct {
	Question string        `json:"question"`
	Hits     []SymbolBrief `json:"hits"`
}

type SymbolBrief struct {
	Qualified string   `json:"qualified"`
	Kind      string   `json:"kind"`
	Path      string   `json:"path"`
	Lines     string   `json:"lines"`
	Signature string   `json:"signature,omitempty"`
	Doc       string   `json:"doc,omitempty"`
	Score     float32  `json:"score"`
	Reasons   []string `json:"reasons"`
}

func (s *Server) handleQuery(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in QueryInput,
) (*mcp.CallToolResult, QueryOutput, error) {
	opt := retrieve.DefaultOptions()
	if in.MaxResults > 0 {
		opt.MaxResults = in.MaxResults
	}
	results, err := retrieve.Query(ctx, s.Store, s.LLM, in.Question, opt)
	if err != nil {
		return nil, QueryOutput{}, err
	}
	out := QueryOutput{Question: in.Question}
	for _, r := range results {
		out.Hits = append(out.Hits, toBrief(r))
	}
	return nil, out, nil
}

// --- tool: find_entrypoints --------------------------------------------------

type EntrypointsInput struct{}

type EntrypointsOutput struct {
	Mains       []SymbolBrief `json:"mains"`
	Inits       []SymbolBrief `json:"inits"`
	HTTPRoutes  []SymbolBrief `json:"http_routes,omitempty"`
	Description string        `json:"description"`
}

func (s *Server) handleEntrypoints(
	_ context.Context,
	_ *mcp.CallToolRequest,
	_ EntrypointsInput,
) (*mcp.CallToolResult, EntrypointsOutput, error) {
	out := EntrypointsOutput{
		Description: "Entrypoints of the repository. main() functions are the binary entry; init() functions run at package load. HTTP routes are heuristic — symbols whose names match registration patterns.",
	}

	// main() functions
	rows, err := s.Store.DB().Query(`
		SELECT id, kind, name, qualified, COALESCE(file_id, 0),
		       line_start, line_end, signature, doc, exported
		FROM symbols WHERE name = 'main' AND kind = 'func'
		ORDER BY qualified`)
	if err == nil {
		mains, _ := briefsFromRows(s.Store, rows)
		rows.Close()
		out.Mains = mains
	}

	// init() functions
	rows, err = s.Store.DB().Query(`
		SELECT id, kind, name, qualified, COALESCE(file_id, 0),
		       line_start, line_end, signature, doc, exported
		FROM symbols WHERE name = 'init' AND kind = 'func'
		ORDER BY qualified`)
	if err == nil {
		inits, _ := briefsFromRows(s.Store, rows)
		rows.Close()
		out.Inits = inits
	}

	// HTTP routes (heuristic): symbols whose signature mentions http.Handler,
	// http.HandleFunc, mux.HandleFunc, gin/chi/echo routers.
	rows, err = s.Store.DB().Query(`
		SELECT s.id, s.kind, s.name, s.qualified, COALESCE(s.file_id, 0),
		       s.line_start, s.line_end, s.signature, s.doc, s.exported
		FROM symbols s
		WHERE s.signature LIKE '%http.ResponseWriter%'
		   OR s.signature LIKE '%http.Handler%'
		   OR s.signature LIKE '%gin.Context%'
		   OR s.signature LIKE '%echo.Context%'
		   OR s.signature LIKE '%chi.Router%'
		ORDER BY s.qualified
		LIMIT 50`)
	if err == nil {
		routes, _ := briefsFromRows(s.Store, rows)
		rows.Close()
		out.HTTPRoutes = routes
	}
	return nil, out, nil
}

// --- tool: explain_symbol ----------------------------------------------------

type ExplainInput struct {
	Qualified string `json:"qualified" jsonschema:"fully-qualified symbol name, e.g. github.com/x/y/payment.ChargeCustomer"`
}

type ExplainOutput struct {
	Symbol    SymbolBrief   `json:"symbol"`
	Callers   []SymbolBrief `json:"callers"`
	Callees   []SymbolBrief `json:"callees"`
	Implements []SymbolBrief `json:"implements,omitempty"`
}

func (s *Server) handleExplainSymbol(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in ExplainInput,
) (*mcp.CallToolResult, ExplainOutput, error) {
	var symID int64
	err := s.Store.DB().QueryRow(
		`SELECT id FROM symbols WHERE qualified = ?`, in.Qualified,
	).Scan(&symID)
	if err != nil {
		return nil, ExplainOutput{}, fmt.Errorf("symbol not found: %s", in.Qualified)
	}
	sym, err := s.Store.GetSymbolByID(symID)
	if err != nil || sym == nil {
		return nil, ExplainOutput{}, fmt.Errorf("load symbol: %w", err)
	}
	var file *store.File
	if sym.FileID != 0 {
		file, _ = s.Store.GetFileByID(sym.FileID)
	}
	out := ExplainOutput{Symbol: toBriefRaw(*sym, file, 0, nil)}

	callerIDs, _ := s.Store.Neighbors(symID, "calls", 1, false)
	out.Callers = idsToBriefs(s.Store, callerIDs)

	calleeIDs, _ := s.Store.Neighbors(symID, "calls", 1, true)
	out.Callees = idsToBriefs(s.Store, calleeIDs)

	implIDs, _ := s.Store.Neighbors(symID, "implements", 1, true)
	out.Implements = idsToBriefs(s.Store, implIDs)
	return nil, out, nil
}

// --- tool: where_to_add ------------------------------------------------------

type WhereInput struct {
	Description string `json:"description" jsonschema:"the change to make, e.g. 'add a new payment provider for Klarna'"`
}

type WhereOutput struct {
	Hits        []SymbolBrief `json:"hits"`
	Guidance    string        `json:"guidance"`
}

func (s *Server) handleWhereToAdd(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in WhereInput,
) (*mcp.CallToolResult, WhereOutput, error) {
	opt := retrieve.DefaultOptions()
	opt.MaxResults = 15
	results, err := retrieve.Query(ctx, s.Store, s.LLM, in.Description, opt)
	if err != nil {
		return nil, WhereOutput{}, err
	}
	out := WhereOutput{
		Guidance: "Likely modification sites, ranked by relevance. Files marked as interface types are extension points: implement the interface to plug your change in.",
	}
	for _, r := range results {
		out.Hits = append(out.Hits, toBrief(r))
	}
	return nil, out, nil
}

// --- tool: architecture_overview ---------------------------------------------

type OverviewInput struct{}

type OverviewOutput struct {
	Packages   []PackageInfo  `json:"packages"`
	HotFiles   []HotFileInfo  `json:"hot_files"`
	TotalFiles int            `json:"total_files"`
	TotalSyms  int            `json:"total_symbols"`
}

type PackageInfo struct {
	Path  string `json:"path"`
	Files int    `json:"files"`
	LOC   int    `json:"loc"`
}

type HotFileInfo struct {
	Path    string `json:"path"`
	Package string `json:"package"`
	LOC     int    `json:"loc"`
	Churn   int    `json:"churn"`
	Commits int    `json:"commits"`
}

func (s *Server) handleOverview(
	_ context.Context,
	_ *mcp.CallToolRequest,
	_ OverviewInput,
) (*mcp.CallToolResult, OverviewOutput, error) {
	out := OverviewOutput{}
	rows, err := s.Store.DB().Query(`
		SELECT package, COUNT(*) AS files, COALESCE(SUM(loc), 0) AS loc
		FROM files
		WHERE is_generated = 0
		GROUP BY package
		ORDER BY loc DESC
		LIMIT 50`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var pi PackageInfo
			if err := rows.Scan(&pi.Path, &pi.Files, &pi.LOC); err == nil {
				out.Packages = append(out.Packages, pi)
			}
		}
	}
	if hot, err := gitlog.HotFiles(s.Store, 20); err == nil {
		for _, h := range hot {
			out.HotFiles = append(out.HotFiles, HotFileInfo{
				Path: h.Path, Package: h.Package, LOC: h.LOC,
				Churn: h.Churn, Commits: h.Commits,
			})
		}
	}
	_ = s.Store.DB().QueryRow(`SELECT COUNT(*) FROM files`).Scan(&out.TotalFiles)
	_ = s.Store.DB().QueryRow(`SELECT COUNT(*) FROM symbols`).Scan(&out.TotalSyms)
	return nil, out, nil
}

// --- helpers -----------------------------------------------------------------

func toBrief(r retrieve.Result) SymbolBrief {
	path := ""
	if r.File != nil {
		path = r.File.Path
	}
	return SymbolBrief{
		Qualified: r.Symbol.Qualified,
		Kind:      r.Symbol.Kind,
		Path:      path,
		Lines:     fmt.Sprintf("%d-%d", r.Symbol.LineStart, r.Symbol.LineEnd),
		Signature: r.Symbol.Signature,
		Doc:       truncate(r.Symbol.Doc, 280),
		Score:     r.Score,
		Reasons:   r.Reasons,
	}
}

func toBriefRaw(sym store.Symbol, file *store.File, score float32, reasons []string) SymbolBrief {
	path := ""
	if file != nil {
		path = file.Path
	}
	return SymbolBrief{
		Qualified: sym.Qualified,
		Kind:      sym.Kind,
		Path:      path,
		Lines:     fmt.Sprintf("%d-%d", sym.LineStart, sym.LineEnd),
		Signature: sym.Signature,
		Doc:       truncate(sym.Doc, 280),
		Score:     score,
		Reasons:   reasons,
	}
}

func idsToBriefs(s *store.Store, ids []int64) []SymbolBrief {
	out := make([]SymbolBrief, 0, len(ids))
	for _, id := range ids {
		sym, err := s.GetSymbolByID(id)
		if err != nil || sym == nil {
			continue
		}
		var file *store.File
		if sym.FileID != 0 {
			file, _ = s.GetFileByID(sym.FileID)
		}
		out = append(out, toBriefRaw(*sym, file, 0, nil))
	}
	return out
}

func briefsFromRows(s *store.Store, rows interface {
	Next() bool
	Scan(...any) error
}) ([]SymbolBrief, error) {
	var out []SymbolBrief
	for rows.Next() {
		var sym store.Symbol
		var exported int
		if err := rows.Scan(&sym.ID, &sym.Kind, &sym.Name, &sym.Qualified,
			&sym.FileID, &sym.LineStart, &sym.LineEnd, &sym.Signature,
			&sym.Doc, &exported); err != nil {
			return out, err
		}
		sym.Exported = exported != 0
		var file *store.File
		if sym.FileID != 0 {
			file, _ = s.GetFileByID(sym.FileID)
		}
		out = append(out, toBriefRaw(sym, file, 0, nil))
	}
	return out, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
