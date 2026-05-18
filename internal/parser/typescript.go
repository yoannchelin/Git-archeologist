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
)

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

	return stats, nil
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
