// diagram.go generates Mermaid diagrams from the code graph.
//
// Two modes:
//   - call_graph: BFS from a named symbol, following calls edges both ways.
//   - package_deps: aggregate cross-package call edges into a package-level graph.
//
// Mermaid renders inline in Claude Desktop, Zed, and most MCP clients —
// this is the "waouh" moment for onboarding demos.
package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DiagramInput is the input schema for the diagram tool.
type DiagramInput struct {
	Symbol string `json:"symbol,omitempty" jsonschema:"for call_graph: fully-qualified symbol name (e.g. github.com/x/y/pkg.MyFunc); for package_deps: a package path to focus on, or omit for repo-wide view"`
	Depth  int    `json:"depth,omitempty"  jsonschema:"expansion depth 1-4 (default 2); only used for call_graph"`
	Kind   string `json:"kind,omitempty"   jsonschema:"call_graph (default) or package_deps"`
}

// DiagramOutput is the structured result returned to the MCP client.
type DiagramOutput struct {
	Mermaid     string `json:"mermaid"`
	NodeCount   int    `json:"node_count"`
	EdgeCount   int    `json:"edge_count"`
	Description string `json:"description"`
}

func (s *Server) handleDiagram(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in DiagramInput,
) (*mcp.CallToolResult, DiagramOutput, error) {
	depth := in.Depth
	if depth <= 0 {
		depth = 2
	}
	if depth > 4 {
		depth = 4
	}
	switch in.Kind {
	case "", "call_graph":
		return s.callGraphDiagram(in.Symbol, depth)
	case "package_deps":
		return s.packageDepsDiagram(in.Symbol)
	default:
		return nil, DiagramOutput{}, fmt.Errorf("unknown kind %q: use call_graph or package_deps", in.Kind)
	}
}

const maxDiagramNodes = 50

// callGraphDiagram builds a flowchart of the calls graph around a symbol.
//
// Strategy: expand callees (outgoing) for `depth` hops, then add callers of
// the root (1 hop incoming). Callers show "who uses this?"; callees show
// "what does this depend on?". Capped at maxDiagramNodes to keep diagrams legible.
func (s *Server) callGraphDiagram(qualified string, depth int) (*mcp.CallToolResult, DiagramOutput, error) {
	if qualified == "" {
		return nil, DiagramOutput{}, fmt.Errorf("call_graph requires a symbol qualified name")
	}
	db := s.Store.DB()

	var rootID int64
	var rootName, rootKind string
	if err := db.QueryRow(
		`SELECT id, name, kind FROM symbols WHERE qualified = ? LIMIT 1`, qualified,
	).Scan(&rootID, &rootName, &rootKind); err != nil {
		return nil, DiagramOutput{}, fmt.Errorf("symbol %q not found in index", qualified)
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

	// BFS outgoing (callees).
	frontier := []int64{rootID}
	for hop := 0; hop < depth && len(frontier) > 0 && len(nodeIDs) < maxDiagramNodes; hop++ {
		ph, args := inPlaceholders(frontier)
		rows, err := db.Query(
			fmt.Sprintf(`SELECT DISTINCT src, dst FROM edges WHERE src IN (%s) AND relation='calls'`, ph),
			args...)
		if err != nil {
			break
		}
		var next []int64
		for rows.Next() {
			var src, dst int64
			if _ = rows.Scan(&src, &dst); true {
				addEdge(src, dst)
				if !nodeIDs[dst] && len(nodeIDs) < maxDiagramNodes {
					nodeIDs[dst] = true
					next = append(next, dst)
				}
			}
		}
		_ = rows.Close()
		frontier = next
	}

	// 1 hop incoming (callers of root only — avoids runaway expansion).
	rows, err := db.Query(
		`SELECT DISTINCT src, dst FROM edges WHERE dst = ? AND relation='calls'`, rootID)
	if err == nil {
		for rows.Next() {
			var src, dst int64
			if _ = rows.Scan(&src, &dst); true {
				addEdge(src, dst)
				if !nodeIDs[src] && len(nodeIDs) < maxDiagramNodes {
					nodeIDs[src] = true
				}
			}
		}
		_ = rows.Close()
	}

	// Fetch labels for all nodes in one round-trip using IN (...).
	type nodeInfo struct{ name, kind string }
	labels := make(map[int64]nodeInfo, len(nodeIDs))
	{
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
			_ = lrows.Close()
		}
	}

	// Render Mermaid. Root node uses a stadium shape to stand out.
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

	return nil, DiagramOutput{
		Mermaid:   sb.String(),
		NodeCount: len(nodeIDs),
		EdgeCount: len(edges),
		Description: fmt.Sprintf(
			"Call graph around %s (%s), depth %d — %d nodes, %d edges",
			rootName, rootKind, depth, len(nodeIDs), len(edges)),
	}, nil
}

// packageDepsDiagram aggregates all cross-package call edges into a
// package-level dependency graph. If focus is a package path, only edges
// touching that package are included.
func (s *Server) packageDepsDiagram(focus string) (*mcp.CallToolResult, DiagramOutput, error) {
	db := s.Store.DB()

	rows, err := db.Query(`
		SELECT DISTINCT f1.package, f2.package
		FROM edges e
		JOIN symbols s1 ON s1.id = e.src
		JOIN symbols s2 ON s2.id = e.dst
		JOIN files   f1 ON f1.id = s1.file_id
		JOIN files   f2 ON f2.id = s2.file_id
		WHERE e.relation = 'calls'
		  AND f1.package != f2.package
		  AND s1.file_id != 0
		  AND s2.file_id != 0
		LIMIT 300`)
	if err != nil {
		return nil, DiagramOutput{}, fmt.Errorf("query package edges: %w", err)
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

	// Assign stable numeric IDs for Mermaid node names.
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

	return nil, DiagramOutput{
		Mermaid:     sb.String(),
		NodeCount:   len(pkgSet),
		EdgeCount:   len(edges),
		Description: desc,
	}, nil
}

// inPlaceholders builds a "?,?,?" string and a []any argument slice for SQL IN clauses.
func inPlaceholders(ids []int64) (string, []any) {
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	return strings.Join(ph, ","), args
}

// mermaidQuote wraps a label in Mermaid double-quotes and escapes interior quotes.
func mermaidQuote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `#quot;`) + `"`
}

// pkgShortName returns the last 2 path segments of a Go package path for
// compact node labels in package diagrams.
func pkgShortName(pkg string) string {
	parts := strings.Split(pkg, "/")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "/")
}
