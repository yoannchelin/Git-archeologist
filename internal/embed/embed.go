// Package embed drives the embedding pipeline.
//
// Decisions worth recording:
//
//   - We embed funcs, methods, types, interfaces, and files. Not packages
//     (too coarse, name is mostly redundant) and not vars/consts (too noisy
//     for the budget). Files act as fallbacks for "no docstring" symbols.
//
//   - The text we embed for a symbol is composite:
//     "{kind} {qualified}\n{signature}\n{doc}\n{first ~80 lines of code}"
//     This captures intent (doc), shape (signature), and structure (code prefix).
//     We deliberately do NOT embed the full body — long bodies dilute the
//     vector and the call graph already gives us reachable context.
//
//   - We are sequential, not parallel. Ollama serialises requests internally
//     and parallelism just causes thrash on a single-GPU laptop. If we want
//     speed later, the right move is batching at the HTTP layer (Ollama 0.1.30+
//     supports multi-input /api/embed), not goroutines.
package embed

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yoannchl/git-archaeologist/internal/llm"
	"github.com/yoannchl/git-archaeologist/internal/store"
)

// Run embeds every eligible symbol in s, persisting vectors to the store.
//
// repoRoot is needed so we can read the symbol's source file to include a
// code prefix in the embedding text. progress is called every N symbols if
// non-nil, useful for CLI status output.
func Run(
	ctx context.Context,
	s *store.Store,
	client *llm.Client,
	repoRoot string,
	progress func(done, total int),
) error {
	symbols, err := selectEmbeddableSymbols(s)
	if err != nil {
		return err
	}
	total := len(symbols)
	for i, sym := range symbols {
		if err := ctx.Err(); err != nil {
			return err
		}
		text, err := buildEmbedText(repoRoot, s, sym)
		if err != nil {
			return fmt.Errorf("build text for %s: %w", sym.Qualified, err)
		}
		vec, err := client.Embed(ctx, text)
		if err != nil {
			return fmt.Errorf("embed %s: %w", sym.Qualified, err)
		}
		if err := s.PutEmbedding(sym.ID, vec, client.EmbedModel); err != nil {
			return fmt.Errorf("store embedding %s: %w", sym.Qualified, err)
		}
		if progress != nil && (i%25 == 0 || i == total-1) {
			progress(i+1, total)
		}
	}
	return s.SetMeta("embed_model", client.EmbedModel)
}

// selectEmbeddableSymbols returns the symbols we want to embed, in a stable order.
func selectEmbeddableSymbols(s *store.Store) ([]store.Symbol, error) {
	rows, err := s.DB().Query(`
		SELECT id, kind, name, qualified, COALESCE(file_id, 0),
		       line_start, line_end, signature, doc, exported
		FROM symbols
		WHERE kind IN ('func','method','type','interface','file')
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Symbol
	for rows.Next() {
		var sym store.Symbol
		var exported int
		if err := rows.Scan(&sym.ID, &sym.Kind, &sym.Name, &sym.Qualified,
			&sym.FileID, &sym.LineStart, &sym.LineEnd, &sym.Signature,
			&sym.Doc, &exported); err != nil {
			return nil, err
		}
		sym.Exported = exported != 0
		out = append(out, sym)
	}
	return out, rows.Err()
}

// buildEmbedText composes the string we send to the embedding model.
func buildEmbedText(repoRoot string, s *store.Store, sym store.Symbol) (string, error) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s %s\n", sym.Kind, sym.Qualified)
	if sym.Signature != "" {
		sb.WriteString(sym.Signature)
		sb.WriteString("\n")
	}
	if sym.Doc != "" {
		sb.WriteString(sym.Doc)
		sb.WriteString("\n")
	}
	// File-kind symbols use the first ~60 LOC as their text. Funcs/methods
	// use their own range. Types use the surrounding lines.
	if sym.FileID == 0 {
		return strings.TrimSpace(sb.String()), nil
	}
	file, err := s.GetFileByID(sym.FileID)
	if err != nil || file == nil {
		return strings.TrimSpace(sb.String()), nil
	}
	start, end := sym.LineStart, sym.LineEnd
	if sym.Kind == "file" {
		start, end = 1, 60
	}
	if end-start > 80 {
		end = start + 80
	}
	snippet, err := readLines(filepath.Join(repoRoot, file.Path), start, end)
	if err == nil && snippet != "" {
		sb.WriteString("\n")
		sb.WriteString(snippet)
	}
	return strings.TrimSpace(sb.String()), nil
}

func readLines(path string, start, end int) (string, error) {
	if start < 1 {
		start = 1
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var sb strings.Builder
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		if lineNo < start {
			continue
		}
		if lineNo > end {
			break
		}
		sb.WriteString(scanner.Text())
		sb.WriteByte('\n')
	}
	return sb.String(), scanner.Err()
}
