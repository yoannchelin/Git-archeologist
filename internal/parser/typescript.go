// typescript.go adds TypeScript/TSX symbol extraction to the archaeologist.
//
// Why no tree-sitter?
//
//	Tree-sitter would give us a proper CST but adds ~20 MB of CGo binaries and
//	a grammar-per-language maintenance burden. For the onboarding use case we
//	only need top-level declarations and local import edges — a regexp scanner
//	over unindented lines is fast, dependency-free, and accurate enough.
//
// What we extract:
//
//	function/async function declarations  → kind=func
//	const arrow functions at column 0     → kind=func
//	class declarations                    → kind=type
//	interface declarations                → kind=interface
//	type alias declarations               → kind=type
//	local import statements               → imports edges between file symbols
//
// Qualified name format: "<relpath>.<SymbolName>" (e.g. "src/api/handler.ts.processPayment")
// Package field: directory of the file (e.g. "src/api")
package parser

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yoannchl/git-archaeologist/internal/store"
)

var (
	reTSFunc      = regexp.MustCompile(`^(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][A-Za-z0-9_$]*)`)
	reTSArrow     = regexp.MustCompile(`^(?:export\s+)?const\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*(?::[^=]+)?\s*=\s*(?:async\s+)?\(`)
	reTSClass     = regexp.MustCompile(`^(?:export\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	reTSInterface = regexp.MustCompile(`^(?:export\s+)?interface\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	reTSTypeAlias = regexp.MustCompile(`^(?:export\s+)?type\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*(?:<[^>]*)?\s*=`)
	reTSImport    = regexp.MustCompile(`from\s+['"](\.[^'"]+)['"]`)

	// reTSCallSite matches any identifier immediately followed by '(', which is
	// the syntactic shape of a function or method call. Word-boundary anchoring
	// means it also matches `this.foo(` and `obj.bar(` (the `.` resets the
	// word boundary, so `foo` / `bar` are captured). Filtered against
	// tsCallSkip to drop language keywords and well-known built-ins.
	reTSCallSite = regexp.MustCompile(`\b([A-Za-z_$][A-Za-z0-9_$]*)\s*\(`)
)

// tsCallSkip is the set of identifiers that look like calls but aren't
// user-defined function calls we care about.
var tsCallSkip = map[string]bool{
	// Control flow / statements
	"if": true, "else": true, "for": true, "while": true, "do": true,
	"switch": true, "case": true, "catch": true, "finally": true,
	"return": true, "throw": true, "typeof": true, "instanceof": true,
	"void": true, "delete": true, "await": true, "async": true,
	"new": true, "import": true, "export": true, "function": true,
	"class": true, "const": true, "let": true, "var": true,
	// Built-in globals & constructors
	"Promise": true, "Array": true, "Object": true, "String": true,
	"Number": true, "Boolean": true, "Symbol": true, "BigInt": true,
	"Error": true, "TypeError": true, "RangeError": true, "SyntaxError": true,
	"Map": true, "Set": true, "WeakMap": true, "WeakSet": true,
	"Math": true, "Date": true, "JSON": true, "RegExp": true,
	"console": true, "process": true, "Buffer": true, "global": true,
	"setTimeout": true, "setInterval": true, "clearTimeout": true,
	"clearInterval": true, "setImmediate": true, "clearImmediate": true,
	"parseInt": true, "parseFloat": true, "isNaN": true, "isFinite": true,
	"encodeURIComponent": true, "decodeURIComponent": true,
	"require": true, "module": true, "exports": true,
	"super": true, "this": true, "self": true, "globalThis": true,
	// Common built-in method names that produce noisy cross-file matches
	// (e.g. `map.get(k)` resolves to an unrelated `get` accessor in a test file).
	"get": true, "set": true, "has": true, "add": true, "clear": true,
	"keys": true, "values": true, "entries": true, "next": true,
	"then": true, "resolve": true, "reject": true,
	"push": true, "pop": true, "shift": true, "unshift": true,
	"map": true, "filter": true, "reduce": true, "find": true, "some": true, "every": true,
	"includes": true, "indexOf": true, "slice": true, "splice": true, "join": true,
	"split": true, "trim": true, "replace": true, "match": true, "toString": true,
	"valueOf": true, "hasOwnProperty": true, "call": true, "apply": true, "bind": true,
	// Common test framework globals
	"describe": true, "it": true, "test": true, "expect": true,
	"beforeEach": true, "afterEach": true, "beforeAll": true, "afterAll": true,
	"jest": true, "vi": true, "spyOn": true,
}

// ParseTS walks the repo for TypeScript and TSX files, inserting symbols and
// import edges into the store. Returns an empty Stats (no error) when the repo
// contains no TypeScript files.
//
// The caller (index.Build) should merge the returned Stats into the overall
// report. Errors on individual files are collected in Stats.Errors rather than
// aborting the whole parse.
func ParseTS(repoRoot string, s *store.Store) (*Stats, error) {
	tsFiles, err := findTSFiles(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("find TypeScript files: %w", err)
	}
	if len(tsFiles) == 0 {
		return &Stats{}, nil
	}

	stats := &Stats{}
	batch, err := s.Begin()
	if err != nil {
		return nil, err
	}

	fileIDs := make(map[string]int64)    // absPath → files.id
	fileSymIDs := make(map[string]int64) // absPath → symbols.id (file-kind symbol)

	// Pass 1: insert files + symbols.
	for _, absPath := range tsFiles {
		relPath := relTo(repoRoot, absPath)
		dir := filepath.ToSlash(filepath.Dir(relPath))
		if dir == "." {
			dir = ""
		}

		src, err := os.ReadFile(absPath)
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Sprintf("read %s: %v", relPath, err))
			continue
		}
		lines := strings.Split(string(src), "\n")

		fid, err := batch.PutFile(store.File{
			Path:     relPath,
			Package:  dir,
			LOC:      len(lines),
			Language: "typescript",
			IsTest:   isTSTestFile(relPath),
		})
		if err != nil {
			_ = batch.Rollback()
			return nil, fmt.Errorf("put file %s: %w", relPath, err)
		}
		fileIDs[absPath] = fid
		stats.Files++

		// File-level symbol: lets the embedding pipeline and import edges reference it.
		fileSymID, err := batch.PutSymbol(store.Symbol{
			Kind:      "file",
			Name:      filepath.Base(relPath),
			Qualified: relPath,
			FileID:    fid,
			LineEnd:   len(lines),
			Exported:  true,
		})
		if err != nil {
			_ = batch.Rollback()
			return nil, fmt.Errorf("put file symbol %s: %w", relPath, err)
		}
		fileSymIDs[absPath] = fileSymID

		// Top-level symbol declarations.
		syms := extractTSSymbols(relPath, lines)
		for _, sym := range syms {
			sym.FileID = fid
			if _, err := batch.PutSymbol(sym); err == nil {
				stats.Symbols++
			}
		}
	}

	if err := batch.Commit(); err != nil {
		return nil, fmt.Errorf("commit ts pass 1: %w", err)
	}

	// Pass 2: local import edges (file → file).
	batch, err = s.Begin()
	if err != nil {
		return nil, err
	}

	for _, absPath := range tsFiles {
		srcSymID, ok := fileSymIDs[absPath]
		if !ok {
			continue
		}
		src, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		for _, imp := range extractTSImports(string(src)) {
			dstAbs := resolveImport(absPath, imp)
			if dstAbs == "" {
				continue
			}
			dstSymID, ok := fileSymIDs[dstAbs]
			if !ok {
				continue
			}
			if err := batch.PutEdge(store.Edge{
				Src: srcSymID, Dst: dstSymID, Relation: "imports", Weight: 1,
			}); err == nil {
				stats.ImportEdges++
			}
		}
	}

	if err := batch.Commit(); err != nil {
		return nil, fmt.Errorf("commit ts pass 2: %w", err)
	}

	// Pass 3: heuristic call edges.
	// Query all func symbols from the TS files we just indexed, then scan each
	// file's function bodies for call-site patterns. One DB round-trip total.
	callersByFile, calleeIndex, err := buildTSCallIndex(s)
	if err == nil && len(calleeIndex) > 0 {
		batch, err = s.Begin()
		if err != nil {
			return nil, err
		}
		for _, absPath := range tsFiles {
			relPath := relTo(repoRoot, absPath)
			callers := callersByFile[relPath]
			if len(callers) == 0 {
				continue
			}
			src, err := os.ReadFile(absPath)
			if err != nil {
				continue
			}
			lines := strings.Split(string(src), "\n")
			dir := filepath.ToSlash(filepath.Dir(relPath))
			edges := extractTSCallEdges(relPath, dir, lines, callers, calleeIndex)
			for _, e := range edges {
				if err := batch.PutEdge(e); err == nil {
					stats.CallEdges++
				}
			}
		}
		if err := batch.Commit(); err != nil {
			return nil, fmt.Errorf("commit ts pass 3: %w", err)
		}
	}

	return stats, nil
}

// tsSymEntry is one entry in the name-based lookup tables for pass 3.
type tsSymEntry struct {
	id      int64
	name    string
	relPath string
	pkg     string // directory component
	line    int
}

// buildTSCallIndex returns two maps built from a single DB query:
//   - callersByFile: relPath → func symbols in that file, sorted by line
//   - calleeIndex:   name → all func symbols with that name (for callee lookup)
func buildTSCallIndex(s *store.Store) (map[string][]tsSymEntry, map[string][]tsSymEntry, error) {
	rows, err := s.DB().Query(`
		SELECT s.id, s.name, s.line_start, f.path, f.package
		FROM symbols s
		JOIN files f ON f.id = s.file_id
		WHERE s.kind = 'func' AND f.language = 'typescript'`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	callersByFile := make(map[string][]tsSymEntry)
	calleeIndex := make(map[string][]tsSymEntry)
	for rows.Next() {
		var e tsSymEntry
		if err := rows.Scan(&e.id, &e.name, &e.line, &e.relPath, &e.pkg); err != nil {
			return nil, nil, err
		}
		callersByFile[e.relPath] = append(callersByFile[e.relPath], e)
		calleeIndex[e.name] = append(calleeIndex[e.name], e)
	}
	return callersByFile, calleeIndex, rows.Err()
}

// extractTSCallEdges scans the function bodies of a TypeScript file and returns
// store.Edge values for detected call sites.
//
// Caller attribution: the lexically nearest preceding top-level function
// declaration owns all call sites until the next top-level declaration. This
// correctly attributes sequential functions and misses only closures / class
// methods — acceptable for the onboarding graph-expansion use case.
//
// Callee resolution priority: same file > same package > first match found.
func extractTSCallEdges(
	relPath, pkg string,
	lines []string,
	callers []tsSymEntry,
	calleeIndex map[string][]tsSymEntry,
) []store.Edge {
	if len(callers) == 0 {
		return nil
	}

	// Build a line→callerID map: a line in caller[i]'s body belongs to
	// caller[i] (sorted by line, so caller[i] owns lines [line_i, line_{i+1})).
	type lineRange struct {
		from, to int // 1-based, to is exclusive (0 = open-ended)
		id       int64
	}
	ranges := make([]lineRange, len(callers))
	for i, c := range callers {
		end := 0
		if i+1 < len(callers) {
			end = callers[i+1].line
		}
		ranges[i] = lineRange{c.line, end, c.id}
	}

	callerIDForLine := func(lineNo int) int64 {
		for _, r := range ranges {
			if lineNo >= r.from && (r.to == 0 || lineNo < r.to) {
				return r.id
			}
		}
		return 0
	}

	resolveCallee := func(name string) int64 {
		candidates := calleeIndex[name]
		if len(candidates) == 0 {
			return 0
		}
		// Prefer same file.
		for _, c := range candidates {
			if c.relPath == relPath {
				return c.id
			}
		}
		// Then same package.
		for _, c := range candidates {
			if c.pkg == pkg {
				return c.id
			}
		}
		return candidates[0].id
	}

	seen := make(map[[2]int64]bool)
	var edges []store.Edge

	for i, raw := range lines {
		lineNo := i + 1
		callerID := callerIDForLine(lineNo)
		if callerID == 0 {
			continue
		}
		// Skip the declaration line itself (col-0 function keyword) to avoid
		// self-edges from the function's own name appearing in its signature.
		if len(raw) > 0 && raw[0] != ' ' && raw[0] != '\t' {
			continue
		}
		matches := reTSCallSite.FindAllStringSubmatch(raw, -1)
		for _, m := range matches {
			name := m[1]
			if tsCallSkip[name] {
				continue
			}
			calleeID := resolveCallee(name)
			if calleeID == 0 || calleeID == callerID {
				continue
			}
			key := [2]int64{callerID, calleeID}
			if seen[key] {
				continue
			}
			seen[key] = true
			edges = append(edges, store.Edge{
				Src: callerID, Dst: calleeID, Relation: "calls", Weight: 1,
			})
		}
	}
	return edges
}

// findTSFiles returns absolute paths to all .ts/.tsx files under root,
// skipping node_modules, dist, build, .git, and hidden directories.
func findTSFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "dist", "build", "out", ".git", ".archaeo", "vendor", "testdata":
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".ts" || ext == ".tsx" {
			out = append(out, path)
		}
		return nil
	})
	return out, err
}

// extractTSSymbols scans lines for top-level TypeScript declarations.
// Only lines starting at column 0 are examined to avoid matching class methods
// or nested functions.
func extractTSSymbols(relPath string, lines []string) []store.Symbol {
	var syms []store.Symbol
	var docBuf strings.Builder
	inBlockComment := false

	qual := func(name string) string { return relPath + "." + name }
	exported := func(line string) bool { return strings.HasPrefix(line, "export ") }

	for i, raw := range lines {
		lineNo := i + 1

		// Only match top-level (column-0) declarations.
		if len(raw) > 0 && (raw[0] == ' ' || raw[0] == '\t') {
			docBuf.Reset()
			continue
		}

		trimmed := strings.TrimSpace(raw)

		// Block comment tracking for JSDoc.
		if !inBlockComment && (strings.HasPrefix(trimmed, "/**") || strings.HasPrefix(trimmed, "/*")) {
			inBlockComment = true
			docBuf.Reset()
			if strings.Contains(trimmed, "*/") {
				inBlockComment = false
				docBuf.WriteString(cleanJSDoc(trimmed))
			}
			continue
		}
		if inBlockComment {
			if strings.Contains(trimmed, "*/") {
				inBlockComment = false
			}
			s := strings.TrimPrefix(trimmed, "* ")
			s = strings.TrimPrefix(s, "*")
			docBuf.WriteString(strings.TrimSpace(s))
			docBuf.WriteByte(' ')
			continue
		}
		if strings.HasPrefix(trimmed, "//") {
			continue
		}

		doc := strings.TrimSpace(docBuf.String())
		docBuf.Reset()

		// Function declaration: function foo / export async function foo
		if m := reTSFunc.FindStringSubmatch(trimmed); m != nil {
			syms = append(syms, store.Symbol{
				Kind: "func", Name: m[1], Qualified: qual(m[1]),
				LineStart: lineNo, LineEnd: lineNo,
				Exported: exported(trimmed), Doc: doc,
				Signature: truncateLine(trimmed),
			})
			continue
		}
		// Arrow function: const foo = (...) =>
		if m := reTSArrow.FindStringSubmatch(trimmed); m != nil {
			syms = append(syms, store.Symbol{
				Kind: "func", Name: m[1], Qualified: qual(m[1]),
				LineStart: lineNo, LineEnd: lineNo,
				Exported: exported(trimmed), Doc: doc,
				Signature: truncateLine(trimmed),
			})
			continue
		}
		// Class declaration
		if m := reTSClass.FindStringSubmatch(trimmed); m != nil {
			syms = append(syms, store.Symbol{
				Kind: "type", Name: m[1], Qualified: qual(m[1]),
				LineStart: lineNo, LineEnd: lineNo,
				Exported: exported(trimmed), Doc: doc,
			})
			continue
		}
		// Interface declaration
		if m := reTSInterface.FindStringSubmatch(trimmed); m != nil {
			syms = append(syms, store.Symbol{
				Kind: "interface", Name: m[1], Qualified: qual(m[1]),
				LineStart: lineNo, LineEnd: lineNo,
				Exported: exported(trimmed), Doc: doc,
			})
			continue
		}
		// Type alias
		if m := reTSTypeAlias.FindStringSubmatch(trimmed); m != nil {
			syms = append(syms, store.Symbol{
				Kind: "type", Name: m[1], Qualified: qual(m[1]),
				LineStart: lineNo, LineEnd: lineNo,
				Exported: exported(trimmed), Doc: doc,
			})
			continue
		}
	}
	return syms
}

// extractTSImports returns relative import specifiers (those starting with .
// or ..) found in src. External package imports are ignored.
func extractTSImports(src string) []string {
	matches := reTSImport.FindAllStringSubmatch(src, -1)
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		imp := m[1]
		if !seen[imp] {
			seen[imp] = true
			out = append(out, imp)
		}
	}
	return out
}

// resolveImport resolves a TypeScript relative import specifier to an absolute
// path. Handles three common patterns:
//   - Bare specifier: `from "./checker"` → checker.ts / checker.tsx
//   - ESM .js alias: `from "./checker.js"` → checker.ts (TS ESM convention)
//   - Directory: `from "./utils"` → utils/index.ts
func resolveImport(fromAbs, imp string) string {
	dir := filepath.Dir(fromAbs)
	base := filepath.Join(dir, filepath.FromSlash(imp))

	// ESM TypeScript convention: import specifiers end in .js but the source
	// file is .ts (e.g. `from "./checker.js"` → checker.ts).
	if strings.HasSuffix(imp, ".js") || strings.HasSuffix(imp, ".jsx") {
		stem := strings.TrimSuffix(strings.TrimSuffix(base, ".jsx"), ".js")
		for _, ext := range []string{".ts", ".tsx"} {
			if p := stem + ext; fileExists(p) {
				return p
			}
		}
	}

	// Bare specifier: append common source extensions.
	for _, ext := range []string{".ts", ".tsx"} {
		if p := base + ext; fileExists(p) {
			return p
		}
	}

	// Directory import: look for index file.
	for _, ext := range []string{".ts", ".tsx"} {
		if p := filepath.Join(base, "index"+ext); fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// isTSTestFile returns true for files that are likely test/spec files.
func isTSTestFile(relPath string) bool {
	parts := strings.Split(filepath.ToSlash(relPath), "/")
	for _, p := range parts {
		switch p {
		case "test", "tests", "__tests__", "spec", "specs", "e2e", "__mocks__":
			return true
		}
	}
	base := filepath.Base(relPath)
	return strings.HasSuffix(base, ".test.ts") ||
		strings.HasSuffix(base, ".spec.ts") ||
		strings.HasSuffix(base, ".test.tsx") ||
		strings.HasSuffix(base, ".spec.tsx")
}

// cleanJSDoc strips /** ... */ markers from a single-line comment block.
func cleanJSDoc(s string) string {
	s = strings.TrimPrefix(s, "/**")
	s = strings.TrimPrefix(s, "/*")
	s = strings.TrimSuffix(s, "*/")
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "* ")
	return s
}

// truncateLine caps a signature line at 120 chars to keep the DB row compact.
func truncateLine(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}
