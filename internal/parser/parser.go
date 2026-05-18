// Package parser walks a Go repository with go/packages and emits a normalized
// stream of symbols and edges to the store layer.
//
// Why go/packages instead of tree-sitter?
//
//	tree-sitter is fast and grammar-only, which means it cannot tell you that
//	`db.Query(...)` actually resolves to `(*sql.DB).Query` — it just sees an
//	identifier. For an archaeology tool that exists to *understand* a repo,
//	losing type information is a non-starter. go/packages gives us the full
//	type system (go/types) including method resolution and interface
//	satisfaction. Slower, heavier, but correct. We fall back to file-level
//	parsing only when a package fails to load.
package parser

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/yoannchl/git-archaeologist/internal/store"
)

// Stats summarizes what the parser found.
type Stats struct {
	Packages    int
	Files       int
	Symbols     int
	CallEdges   int
	ImplEdges   int
	ImportEdges int
	EmbedEdges  int
	Errors      []string
}

// Parse loads all Go packages under repoRoot and writes symbols + edges to s.
//
// The function is intentionally single-shot: it tears down and rebuilds the
// portion of the graph it owns. Incremental indexing is a follow-up; getting
// the model right matters more than incrementality at MVP.
func Parse(repoRoot string, s *store.Store) (*Stats, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedTypes | packages.NeedSyntax |
			packages.NeedTypesInfo | packages.NeedDeps,
		Dir:   repoRoot,
		Tests: false, // tests double the load time; we add them in S2
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("packages.Load: %w", err)
	}

	stats := &Stats{}
	for _, pkg := range pkgs {
		for _, e := range pkg.Errors {
			stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %s", pkg.PkgPath, e.Msg))
		}
	}

	// Pass 1: insert files + top-level symbols. We must do this before edges,
	// because edges reference symbol IDs we don't have yet.
	symIDs := make(map[types.Object]int64) // *types.Func/TypeName -> symbol id
	fileIDs := make(map[string]int64)      // absolute file path -> file id
	pkgSymIDs := make(map[string]int64)    // import path -> package symbol id

	batch, err := s.Begin()
	if err != nil {
		return nil, err
	}

	for _, pkg := range pkgs {
		if pkg.Name == "" {
			continue
		}
		stats.Packages++
		pkgSymID, err := batch.PutSymbol(store.Symbol{
			Kind:      "package",
			Name:      pkg.Name,
			Qualified: pkg.PkgPath,
			Exported:  true,
		})
		if err != nil {
			_ = batch.Rollback()
			return nil, fmt.Errorf("put package symbol: %w", err)
		}
		pkgSymIDs[pkg.PkgPath] = pkgSymID

		for _, file := range pkg.Syntax {
			pos := pkg.Fset.Position(file.Pos())
			absPath := pos.Filename
			relPath := relTo(repoRoot, absPath)
			loc := pkg.Fset.Position(file.End()).Line
			fid, err := batch.PutFile(store.File{
				Path:        relPath,
				Package:     pkg.PkgPath,
				LOC:         loc,
				IsTest:      strings.HasSuffix(relPath, "_test.go"),
				IsGenerated: isGenerated(file),
			})
			if err != nil {
				_ = batch.Rollback()
				return nil, fmt.Errorf("put file %s: %w", relPath, err)
			}
			fileIDs[absPath] = fid
			stats.Files++

			// Each file is also a symbol so we can attach embeddings/Git data to it.
			fileSymID, err := batch.PutSymbol(store.Symbol{
				Kind:      "file",
				Name:      filepath.Base(relPath),
				Qualified: relPath,
				FileID:    fid,
				LineEnd:   loc,
				Exported:  true,
			})
			if err != nil {
				_ = batch.Rollback()
				return nil, err
			}
			// package contains file
			if err := batch.PutEdge(store.Edge{
				Src: pkgSymID, Dst: fileSymID, Relation: "contains", Weight: 1,
			}); err != nil {
				_ = batch.Rollback()
				return nil, err
			}
		}
	}

	// Pass 1b: top-level declarations (funcs, types, vars, consts).
	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue
		}
		for _, file := range pkg.Syntax {
			absPath := pkg.Fset.Position(file.Pos()).Filename
			fid := fileIDs[absPath]
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					id, err := emitFunc(batch, pkg, d, fid)
					if err != nil {
						_ = batch.Rollback()
						return nil, err
					}
					if obj := pkg.TypesInfo.Defs[d.Name]; obj != nil {
						symIDs[obj] = id
					}
					stats.Symbols++
				case *ast.GenDecl:
					if err := emitGenDecl(batch, pkg, d, fid, symIDs, &stats.Symbols); err != nil {
						_ = batch.Rollback()
						return nil, err
					}
				}
			}
		}
	}

	if err := batch.Commit(); err != nil {
		return nil, fmt.Errorf("commit pass 1: %w", err)
	}

	// Pass 2: edges. Separate transaction so a parse error in one package
	// doesn't poison the whole index.
	batch, err = s.Begin()
	if err != nil {
		return nil, err
	}

	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue
		}
		// call edges — use pre-computed function ranges per file to avoid O(n²)
		// AST walks (old enclosingFunc re-walked the file for every call site).
		for _, file := range pkg.Syntax {
			funcRanges := buildFuncRanges(file, pkg.TypesInfo)
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := resolveCallee(call.Fun, pkg.TypesInfo)
				if callee == nil {
					return true
				}
				caller := enclosingFuncFast(funcRanges, call.Pos())
				if caller == nil {
					return true
				}
				srcID, okSrc := symIDs[caller]
				dstID, okDst := symIDs[callee]
				if !okSrc || !okDst {
					return true
				}
				if err := batch.PutEdge(store.Edge{
					Src: srcID, Dst: dstID, Relation: "calls", Weight: 1,
				}); err == nil {
					stats.CallEdges++
				}
				return true
			})
		}

		// interface implementations: for every defined interface in this pkg,
		// scan all packages for concrete types that satisfy it.
		scope := pkg.Types.Scope()
		for _, name := range scope.Names() {
			obj := scope.Lookup(name)
			tn, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}
			iface, ok := tn.Type().Underlying().(*types.Interface)
			if !ok || iface.NumMethods() == 0 {
				continue
			}
			ifaceID, ok := symIDs[obj]
			if !ok {
				continue
			}
			// Check every named type in every package we loaded.
			for _, other := range pkgs {
				if other.Types == nil {
					continue
				}
				oScope := other.Types.Scope()
				for _, oName := range oScope.Names() {
					oObj := oScope.Lookup(oName)
					oTN, ok := oObj.(*types.TypeName)
					if !ok || oTN == tn {
						continue
					}
					// Check both T and *T.
					for _, t := range []types.Type{oTN.Type(), types.NewPointer(oTN.Type())} {
						if types.Implements(t, iface) {
							implID, ok := symIDs[oObj]
							if !ok {
								continue
							}
							if err := batch.PutEdge(store.Edge{
								Src: implID, Dst: ifaceID, Relation: "implements", Weight: 1,
							}); err == nil {
								stats.ImplEdges++
							}
							break
						}
					}
				}
			}
		}

		// imports edges: package → imported package (only within indexed packages).
		srcPkgID, hasSrc := pkgSymIDs[pkg.PkgPath]
		if hasSrc {
			for importPath := range pkg.Imports {
				dstPkgID, ok := pkgSymIDs[importPath]
				if !ok {
					continue // external package, not indexed
				}
				if err2 := batch.PutEdge(store.Edge{
					Src: srcPkgID, Dst: dstPkgID, Relation: "imports", Weight: 1,
				}); err2 == nil {
					stats.ImportEdges++
				} else {
					stats.Errors = append(stats.Errors, fmt.Sprintf("imports edge %s→%s (src=%d,dst=%d): %v", pkg.PkgPath, importPath, srcPkgID, dstPkgID, err2))
				}
			}
		}

		// embeds edges: struct → embedded type (anonymous fields).
		pkgScope := pkg.Types.Scope()
		for _, name := range pkgScope.Names() {
			obj := pkgScope.Lookup(name)
			tn, ok := obj.(*types.TypeName)
			if !ok {
				continue
			}
			st, ok := tn.Type().Underlying().(*types.Struct)
			if !ok {
				continue
			}
			structID, ok := symIDs[obj]
			if !ok {
				continue
			}
			for i := 0; i < st.NumFields(); i++ {
				field := st.Field(i)
				if !field.Embedded() {
					continue
				}
				underlying := field.Type()
				if ptr, ok2 := underlying.(*types.Pointer); ok2 {
					underlying = ptr.Elem()
				}
				named, ok := underlying.(*types.Named)
				if !ok {
					continue
				}
				embeddedID, ok := symIDs[named.Obj()]
				if !ok {
					continue
				}
				if err := batch.PutEdge(store.Edge{
					Src: structID, Dst: embeddedID, Relation: "embeds", Weight: 1,
				}); err == nil {
					stats.EmbedEdges++
				}
			}
		}
	}

	if err := batch.Commit(); err != nil {
		return nil, fmt.Errorf("commit pass 2: %w", err)
	}
	return stats, nil
}

// emitFunc handles a top-level func or method declaration.
func emitFunc(b *store.BatchInsert, pkg *packages.Package, fn *ast.FuncDecl, fileID int64) (int64, error) {
	pos := pkg.Fset.Position(fn.Pos())
	end := pkg.Fset.Position(fn.End())

	obj := pkg.TypesInfo.Defs[fn.Name]
	if obj == nil {
		return 0, nil
	}
	kind := "func"
	qualified := pkg.PkgPath + "." + fn.Name.Name
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		kind = "method"
		recvName := receiverTypeName(fn.Recv.List[0].Type)
		qualified = pkg.PkgPath + "." + recvName + "." + fn.Name.Name
	}

	sig := ""
	if fn.Type != nil {
		sig = renderFuncSig(fn)
	}
	doc := ""
	if fn.Doc != nil {
		doc = fn.Doc.Text()
	}
	return b.PutSymbol(store.Symbol{
		Kind:      kind,
		Name:      fn.Name.Name,
		Qualified: qualified,
		FileID:    fileID,
		LineStart: pos.Line,
		LineEnd:   end.Line,
		Signature: sig,
		Doc:       strings.TrimSpace(doc),
		Exported:  fn.Name.IsExported(),
	})
}

// emitGenDecl handles type/var/const groups.
func emitGenDecl(
	b *store.BatchInsert,
	pkg *packages.Package,
	d *ast.GenDecl,
	fileID int64,
	symIDs map[types.Object]int64,
	count *int,
) error {
	for _, spec := range d.Specs {
		switch sp := spec.(type) {
		case *ast.TypeSpec:
			obj := pkg.TypesInfo.Defs[sp.Name]
			if obj == nil {
				continue
			}
			kind := "type"
			if _, ok := sp.Type.(*ast.InterfaceType); ok {
				kind = "interface"
			}
			doc := ""
			if d.Doc != nil {
				doc = d.Doc.Text()
			}
			pos := pkg.Fset.Position(sp.Pos())
			end := pkg.Fset.Position(sp.End())
			id, err := b.PutSymbol(store.Symbol{
				Kind:      kind,
				Name:      sp.Name.Name,
				Qualified: pkg.PkgPath + "." + sp.Name.Name,
				FileID:    fileID,
				LineStart: pos.Line,
				LineEnd:   end.Line,
				Doc:       strings.TrimSpace(doc),
				Exported:  sp.Name.IsExported(),
			})
			if err != nil {
				return err
			}
			symIDs[obj] = id
			*count++
		case *ast.ValueSpec:
			for _, n := range sp.Names {
				obj := pkg.TypesInfo.Defs[n]
				if obj == nil {
					continue
				}
				kind := "var"
				if d.Tok == token.CONST {
					kind = "const"
				}
				pos := pkg.Fset.Position(n.Pos())
				id, err := b.PutSymbol(store.Symbol{
					Kind:      kind,
					Name:      n.Name,
					Qualified: pkg.PkgPath + "." + n.Name,
					FileID:    fileID,
					LineStart: pos.Line,
					LineEnd:   pos.Line,
					Exported:  n.IsExported(),
				})
				if err != nil {
					return err
				}
				symIDs[obj] = id
				*count++
			}
		}
	}
	return nil
}

// resolveCallee returns the *types.Func target of a call expression, or nil
// if the call cannot be resolved (e.g. dynamic dispatch through a func value).
func resolveCallee(fun ast.Expr, info *types.Info) types.Object {
	switch f := fun.(type) {
	case *ast.Ident:
		return info.Uses[f]
	case *ast.SelectorExpr:
		return info.Uses[f.Sel]
	}
	return nil
}

// funcRange is a half-open position range for one top-level function declaration.
type funcRange struct {
	start, end token.Pos
	obj        types.Object
}

// buildFuncRanges collects function position ranges from a file's top-level
// declarations. Called once per file so we don't re-walk for every call-site.
func buildFuncRanges(file *ast.File, info *types.Info) []funcRange {
	out := make([]funcRange, 0, len(file.Decls))
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if obj := info.Defs[fn.Name]; obj != nil {
			out = append(out, funcRange{fn.Pos(), fn.End(), obj})
		}
	}
	return out
}

// enclosingFuncFast returns the function that contains pos using the
// pre-computed ranges. O(F) where F = top-level function count in the file.
func enclosingFuncFast(ranges []funcRange, pos token.Pos) types.Object {
	for _, r := range ranges {
		if r.start <= pos && pos <= r.end {
			return r.obj
		}
	}
	return nil
}

// receiverTypeName extracts the bare type name from a method receiver,
// stripping pointer indirection and type parameters.
func receiverTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return receiverTypeName(t.X)
	case *ast.IndexExpr:
		return receiverTypeName(t.X)
	case *ast.IndexListExpr:
		return receiverTypeName(t.X)
	}
	return "_"
}

// renderFuncSig produces a compact one-line signature for the symbol row.
// We deliberately don't include the body or doc — they live in their own
// columns.
func renderFuncSig(fn *ast.FuncDecl) string {
	var sb strings.Builder
	sb.WriteString("func ")
	if fn.Recv != nil && len(fn.Recv.List) > 0 {
		sb.WriteString("(")
		sb.WriteString(receiverTypeName(fn.Recv.List[0].Type))
		sb.WriteString(") ")
	}
	sb.WriteString(fn.Name.Name)
	sb.WriteString("(...)")
	return sb.String()
}

// isGenerated checks for the standard "DO NOT EDIT" marker in the first
// few lines of a file. This is the canonical signal documented in
// https://golang.org/s/generatedcode.
func isGenerated(file *ast.File) bool {
	for _, cg := range file.Comments {
		// Generated marker must appear before the package clause.
		if cg.Pos() >= file.Package {
			break
		}
		for _, c := range cg.List {
			if strings.Contains(c.Text, "DO NOT EDIT") {
				return true
			}
		}
	}
	return false
}

// ParsePackage re-indexes a single Go package identified by its import path.
//
// Unlike Parse (which walks ./...), this loads only the requested package
// (plus its transitive deps for type resolution) and updates only that
// package's files and symbols. Incoming call edges from other packages are
// left untouched; outgoing intra-package call edges are regenerated.
//
// Cross-package call edges from this package to others are not regenerated
// here — they will be correct after a full `Parse` run. For day-to-day
// incremental updates (function bodies, signatures, docs) this is fine.
func ParsePackage(repoRoot, pkgPath string, s *store.Store) (*Stats, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedTypes | packages.NeedSyntax |
			packages.NeedTypesInfo | packages.NeedDeps,
		Dir:   repoRoot,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, pkgPath)
	if err != nil {
		return nil, fmt.Errorf("packages.Load %s: %w", pkgPath, err)
	}

	var target *packages.Package
	for _, p := range pkgs {
		if p.PkgPath == pkgPath {
			target = p
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("package %q not found after load", pkgPath)
	}

	stats := &Stats{}
	symIDs := make(map[types.Object]int64)
	fileIDs := make(map[string]int64)

	batch, err := s.Begin()
	if err != nil {
		return nil, err
	}

	// Pass 1: package symbol + file rows.
	stats.Packages++
	pkgSymID, err := batch.PutSymbol(store.Symbol{
		Kind: "package", Name: target.Name, Qualified: target.PkgPath, Exported: true,
	})
	if err != nil {
		_ = batch.Rollback()
		return nil, fmt.Errorf("put package symbol: %w", err)
	}
	for _, file := range target.Syntax {
		pos := target.Fset.Position(file.Pos())
		absPath := pos.Filename
		relPath := relTo(repoRoot, absPath)
		loc := target.Fset.Position(file.End()).Line
		fid, err := batch.PutFile(store.File{
			Path:        relPath,
			Package:     target.PkgPath,
			LOC:         loc,
			IsTest:      strings.HasSuffix(relPath, "_test.go"),
			IsGenerated: isGenerated(file),
		})
		if err != nil {
			_ = batch.Rollback()
			return nil, fmt.Errorf("put file %s: %w", relPath, err)
		}
		fileIDs[absPath] = fid
		stats.Files++
		fileSymID, err := batch.PutSymbol(store.Symbol{
			Kind: "file", Name: filepath.Base(relPath), Qualified: relPath,
			FileID: fid, LineEnd: loc, Exported: true,
		})
		if err != nil {
			_ = batch.Rollback()
			return nil, err
		}
		if err := batch.PutEdge(store.Edge{Src: pkgSymID, Dst: fileSymID, Relation: "contains", Weight: 1}); err != nil {
			_ = batch.Rollback()
			return nil, err
		}
	}

	// Pass 1b: top-level declarations.
	if target.TypesInfo != nil {
		for _, file := range target.Syntax {
			absPath := target.Fset.Position(file.Pos()).Filename
			fid := fileIDs[absPath]
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					id, err := emitFunc(batch, target, d, fid)
					if err != nil {
						_ = batch.Rollback()
						return nil, err
					}
					if obj := target.TypesInfo.Defs[d.Name]; obj != nil {
						symIDs[obj] = id
					}
					stats.Symbols++
				case *ast.GenDecl:
					if err := emitGenDecl(batch, target, d, fid, symIDs, &stats.Symbols); err != nil {
						_ = batch.Rollback()
						return nil, err
					}
				}
			}
		}
	}

	if err := batch.Commit(); err != nil {
		return nil, fmt.Errorf("commit pass 1: %w", err)
	}

	// Pass 2: intra-package call edges only.
	// Cross-package callees aren't in symIDs so they are silently skipped.
	batch, err = s.Begin()
	if err != nil {
		return nil, err
	}
	if target.TypesInfo != nil {
		for _, file := range target.Syntax {
			funcRanges := buildFuncRanges(file, target.TypesInfo)
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				callee := resolveCallee(call.Fun, target.TypesInfo)
				if callee == nil {
					return true
				}
				caller := enclosingFuncFast(funcRanges, call.Pos())
				if caller == nil {
					return true
				}
				srcID, okSrc := symIDs[caller]
				dstID, okDst := symIDs[callee]
				if !okSrc || !okDst {
					return true
				}
				if err := batch.PutEdge(store.Edge{Src: srcID, Dst: dstID, Relation: "calls", Weight: 1}); err == nil {
					stats.CallEdges++
				}
				return true
			})
		}
	}
	if err := batch.Commit(); err != nil {
		return nil, fmt.Errorf("commit pass 2: %w", err)
	}
	return stats, nil
}

// relTo returns p relative to base, falling back to p if relativization fails.
func relTo(base, p string) string {
	r, err := filepath.Rel(base, p)
	if err != nil {
		return p
	}
	return r
}
