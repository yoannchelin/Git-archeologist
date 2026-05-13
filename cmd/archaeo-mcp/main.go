// Command archaeo-mcp starts the Git Archaeologist MCP server.
//
// The server speaks MCP over stdio, so it can be plugged into Claude Desktop,
// Zed, Cursor, or anything else that loads MCP servers via subprocess.
//
// Configuration is via CLI flags:
//
//	archaeo-mcp --repo /path/to/repo
//	            --ollama http://127.0.0.1:11434
//	            --chat-model qwen2.5-coder:14b
//	            --embed-model nomic-embed-text
//
// The repo must have been indexed first with `archaeo index`.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yoannchl/git-archaeologist/internal/index"
	"github.com/yoannchl/git-archaeologist/internal/llm"
	"github.com/yoannchl/git-archaeologist/internal/mcpserver"
	"github.com/yoannchl/git-archaeologist/internal/store"
)

const version = "0.1.0"

func main() {
	repo := flag.String("repo", ".", "path to the repo root (must be pre-indexed)")
	ollama := flag.String("ollama", "http://127.0.0.1:11434", "Ollama base URL")
	chatModel := flag.String("chat-model", "qwen2.5-coder:14b", "chat model")
	embedModel := flag.String("embed-model", "nomic-embed-text", "embedding model")
	watchFlag := flag.Bool("watch", true, "watch for .go file changes and incrementally re-index")
	flag.Parse()

	// Important: log to stderr only. stdout is the MCP wire.
	log.SetOutput(os.Stderr)
	log.SetPrefix("archaeo-mcp: ")

	root, err := filepath.Abs(*repo)
	if err != nil {
		log.Fatalf("resolve repo path: %v", err)
	}
	st, err := store.Open(root)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// Sanity check: index must exist.
	var n int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM symbols`).Scan(&n)
	if n == 0 {
		log.Fatalf("no symbols in index at %s — run `archaeo index --repo %s` first", st.Path(), root)
	}
	log.Printf("loaded index: %d symbols", n)

	client := llm.New(*ollama, *chatModel, *embedModel)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if *watchFlag {
		if err := index.StartWatcher(ctx, root, st, nil, log.Default()); err != nil {
			log.Printf("watcher disabled: %v", err)
		}
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "git-archaeologist",
		Version: version,
	}, nil)

	app := &mcpserver.Server{Store: st, LLM: client, RepoRoot: root}
	app.Register(srv)

	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "mcp server: %v\n", err)
		os.Exit(1)
	}
}
