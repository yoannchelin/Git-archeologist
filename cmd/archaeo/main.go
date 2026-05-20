// Command archaeo indexes a Go repository for the Git Archaeologist.
//
// Usage:
//
//	archaeo index [--repo PATH] [--no-embed] [--no-git] [--ollama URL]
//	              [--chat-model NAME] [--embed-model NAME]
//	archaeo info  [--repo PATH]
//	archaeo query [--repo PATH] "your question"
//	archaeo serve [--repo PATH] [--port 8080]
//
// The MCP server lives in cmd/archaeo-mcp and reads from the same DB.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yoannchl/git-archaeologist/internal/index"
	"github.com/yoannchl/git-archaeologist/internal/llm"
	"github.com/yoannchl/git-archaeologist/internal/retrieve"
	"github.com/yoannchl/git-archaeologist/internal/store"
	"github.com/yoannchl/git-archaeologist/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "index":
		runIndex(args)
	case "info":
		runInfo(args)
	case "query":
		runQuery(args)
	case "serve":
		runServe(args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `archaeo — Git Archaeologist CLI

Subcommands:
  index   Build or refresh the index for a repo
  info    Print stats about an existing index
  query   Run an ad-hoc retrieval query (debug aid)
  serve   Start the web dashboard (opens in browser)

Run "archaeo <subcommand> -h" for flags.
`)
}

func runIndex(args []string) {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	repo := fs.String("repo", ".", "path to the repo root")
	noEmbed := fs.Bool("no-embed", false, "skip embeddings (faster, retrieval falls back to FTS+graph)")
	noGit := fs.Bool("no-git", false, "skip git history ingestion")
	withTests := fs.Bool("with-tests", false, "index _test.go files (slower parse, but tests document usage)")
	fast := fs.Bool("fast", false, "skip type-checking (no call/impl edges, ~3-5× faster on dep-heavy repos)")
	ollama := fs.String("ollama", "http://127.0.0.1:11434", "Ollama base URL")
	chatModel := fs.String("chat-model", "qwen2.5-coder:14b", "Ollama model for chat (used by MCP server, not by index)")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama model for embeddings")
	maxCommits := fs.Int("max-commits", 5000, "cap on git history traversal")
	_ = fs.Parse(args)

	root, err := filepath.Abs(*repo)
	if err != nil {
		die("resolve repo path: %v", err)
	}
	s, err := store.Open(root)
	if err != nil {
		die("open store: %v", err)
	}
	defer s.Close()

	var client *llm.Client
	if !*noEmbed {
		client = llm.New(*ollama, *chatModel, *embedModel)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	opt := index.Options{
		WithGit:        !*noGit,
		WithEmbeddings: !*noEmbed,
		WithTests:      *withTests,
		Fast:           *fast,
		MaxCommits:     *maxCommits,
	}

	rep, err := index.Build(ctx, root, s, client, opt, func(stage string, done, total int) {
		fmt.Fprintf(os.Stderr, "\r[%s] %d/%d ", stage, done, total)
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		die("index: %v", err)
	}
	fmt.Printf("Index built in %s\n", rep.Duration.Round(1e6))
	if rep.ParseStats != nil {
		fmt.Printf("  packages: %d\n  files: %d\n  symbols: %d\n  call edges: %d\n  spawn edges: %d\n  schedule edges: %d\n  impl edges: %d\n  import edges: %d\n  embed edges: %d\n",
			rep.ParseStats.Packages, rep.ParseStats.Files, rep.ParseStats.Symbols,
			rep.ParseStats.CallEdges, rep.ParseStats.SpawnEdges, rep.ParseStats.ScheduleEdges,
			rep.ParseStats.ImplEdges, rep.ParseStats.ImportEdges, rep.ParseStats.EmbedEdges)
	}
	fmt.Printf("  commits: %d\n  embedded: %d\n", rep.Commits, rep.Embedded)
	if n := len(rep.ParseErrors); n > 0 {
		fmt.Fprintf(os.Stderr, "(%d non-fatal errors; first one: %s)\n", n, rep.ParseErrors[0])
	}
}

func runInfo(args []string) {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	repo := fs.String("repo", ".", "path to the repo root")
	_ = fs.Parse(args)

	root, _ := filepath.Abs(*repo)
	s, err := store.Open(root)
	if err != nil {
		die("open store: %v", err)
	}
	defer s.Close()

	var nFiles, nSyms, nEdges, nEmb int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM files`).Scan(&nFiles)
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM symbols`).Scan(&nSyms)
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM edges`).Scan(&nEdges)
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM embeddings`).Scan(&nEmb)

	last, _, _ := s.GetMeta("last_index")
	model, _, _ := s.GetMeta("embed_model")

	fmt.Printf("Index at: %s\n", s.Path())
	fmt.Printf("Last indexed: %s\n", or(last, "never"))
	fmt.Printf("Embedding model: %s\n", or(model, "(none)"))
	fmt.Printf("Files: %d  Symbols: %d  Edges: %d  Embedded: %d\n", nFiles, nSyms, nEdges, nEmb)
}

func runQuery(args []string) {
	fs := flag.NewFlagSet("query", flag.ExitOnError)
	repo := fs.String("repo", ".", "path to the repo root")
	ollama := fs.String("ollama", "http://127.0.0.1:11434", "Ollama base URL")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama embedding model")
	max := fs.Int("max", 15, "max results")
	_ = fs.Parse(args)

	q := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if q == "" {
		die("query: missing question")
	}
	root, _ := filepath.Abs(*repo)
	s, err := store.Open(root)
	if err != nil {
		die("open store: %v", err)
	}
	defer s.Close()
	client := llm.New(*ollama, "", *embedModel)

	opt := retrieve.DefaultOptions()
	opt.MaxResults = *max
	hits, err := retrieve.Query(context.Background(), s, client, q, opt)
	if err != nil {
		die("query: %v", err)
	}
	for i, h := range hits {
		path := ""
		if h.File != nil {
			path = h.File.Path
		}
		fmt.Printf("%2d. [%5.2f] %s  (%s)\n", i+1, h.Score, h.Symbol.Qualified, h.Symbol.Kind)
		if path != "" {
			fmt.Printf("     %s:%d\n", path, h.Symbol.LineStart)
		}
		if h.Symbol.Doc != "" {
			doc := h.Symbol.Doc
			if len(doc) > 160 {
				doc = doc[:160] + "…"
			}
			fmt.Printf("     %s\n", doc)
		}
		fmt.Printf("     reasons: %s\n\n", strings.Join(h.Reasons, ", "))
	}
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	repo := fs.String("repo", ".", "path to the repo root")
	port := fs.Int("port", 8080, "HTTP port to listen on")
	ollama := fs.String("ollama", "http://127.0.0.1:11434", "Ollama base URL")
	embedModel := fs.String("embed-model", "nomic-embed-text", "Ollama embedding model")
	_ = fs.Parse(args)

	root, err := filepath.Abs(*repo)
	if err != nil {
		die("resolve repo path: %v", err)
	}
	s, err := store.Open(root)
	if err != nil {
		die("open store: %v", err)
	}
	defer s.Close()

	client := llm.New(*ollama, "", *embedModel)
	srv := web.New(s, client)

	addr := fmt.Sprintf(":%d", *port)
	url := fmt.Sprintf("http://localhost:%d", *port)
	fmt.Printf("Git Archaeologist dashboard → %s\n", url)
	fmt.Printf("Repo: %s\nPress Ctrl+C to stop.\n", root)

	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		_ = httpSrv.Shutdown(context.Background())
	}()

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		die("serve: %v", err)
	}
}

func or(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "archaeo: "+format+"\n", args...)
	os.Exit(1)
}
