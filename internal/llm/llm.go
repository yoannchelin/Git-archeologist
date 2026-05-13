// Package llm wraps the local LLM provider used by the archaeologist.
//
// At MVP we only target Ollama because its HTTP API is the closest thing to
// a portable contract in the local-LLM world. The Client type is small and
// the interface methods (Embed, Chat) are the seam where llama.cpp or a
// remote API can be plugged in later.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to a local Ollama instance.
type Client struct {
	BaseURL    string         // typically http://127.0.0.1:11434
	ChatModel  string         // e.g. "qwen2.5-coder:14b"
	EmbedModel string         // e.g. "nomic-embed-text"
	HTTP       *http.Client
}

// New returns a Client with sensible defaults.
//
// The HTTP timeout is generous (5 min) because cold-start of a 14B local
// model on first request can exceed a minute on a laptop.
func New(baseURL, chatModel, embedModel string) *Client {
	return &Client{
		BaseURL:    baseURL,
		ChatModel:  chatModel,
		EmbedModel: embedModel,
		HTTP:       &http.Client{Timeout: 5 * time.Minute},
	}
}

// Embed returns the embedding vector for `text`.
//
// We call Ollama's /api/embeddings endpoint. Some recent Ollama versions
// have moved this to /api/embed (plural input); we use the older endpoint
// because it's stable across versions back to 0.1.x.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{
		"model":  c.EmbedModel,
		"prompt": text,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embed http %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embed decode: %w", err)
	}
	if len(out.Embedding) == 0 {
		return nil, fmt.Errorf("embed: empty vector returned")
	}
	return out.Embedding, nil
}

// ChatMessage is one turn of a chat exchange.
type ChatMessage struct {
	Role    string `json:"role"`    // "system" | "user" | "assistant"
	Content string `json:"content"`
}

// Chat sends a multi-turn conversation and returns the assistant's reply.
//
// We do not stream at MVP; the MCP server already aggregates tool results
// before sending them back, so a single blocking call keeps the code simple.
func (c *Client) Chat(ctx context.Context, msgs []ChatMessage) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":    c.ChatModel,
		"messages": msgs,
		"stream":   false,
		"options": map[string]any{
			// Low temperature: archaeology needs grounded, deterministic answers,
			// not creative ones.
			"temperature": 0.1,
			"num_ctx":     8192,
		},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("chat http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("chat http %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Message ChatMessage `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("chat decode: %w", err)
	}
	return out.Message.Content, nil
}
