package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// LLMAPIAdapter calls the Anthropic Messages API directly: no tools, no workdir. Ideal
// for pure-conversation roles (the PRD interviewer).
type LLMAPIAdapter struct {
	BaseURL   string
	Model     string
	APIKeyEnv string
	MaxTokens int
	Client    *http.Client
}

func NewLLMAPI(model, apiKeyEnv string) *LLMAPIAdapter {
	if model == "" {
		model = "claude-sonnet-5-5"
	}
	if apiKeyEnv == "" {
		apiKeyEnv = "ANTHROPIC_API_KEY"
	}
	return &LLMAPIAdapter{BaseURL: "https://api.anthropic.com", Model: model, APIKeyEnv: apiKeyEnv, MaxTokens: 4096}
}

func (a *LLMAPIAdapter) Name() string { return "llmapi" }
func (a *LLMAPIAdapter) Capabilities() Capabilities {
	// Tool-less, so trivially "read-only"; no native schema enforcement.
	return Capabilities{ReadOnlySandbox: true}
}

func (a *LLMAPIAdapter) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	key := req.Env[a.APIKeyEnv]
	if key == "" {
		key = os.Getenv(a.APIKeyEnv)
	}
	if key == "" {
		return RunResult{ExitCode: -1}, fmt.Errorf("llmapi: environment variable %s is not set", a.APIKeyEnv)
	}
	maxTokens := a.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	body, _ := json.Marshal(map[string]any{
		"model":      a.Model,
		"max_tokens": maxTokens,
		"system":     req.SystemPrompt,
		"messages":   []map[string]string{{"role": "user", "content": req.Prompt}},
	})
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.BaseURL, "/")+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return RunResult{ExitCode: -1}, fmt.Errorf("llmapi: %w", err)
	}
	hreq.Header.Set("content-type", "application/json")
	hreq.Header.Set("x-api-key", key)
	hreq.Header.Set("anthropic-version", "2023-06-01")
	c := a.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(hreq)
	if err != nil {
		return RunResult{ExitCode: -1}, fmt.Errorf("llmapi: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return RunResult{ExitCode: resp.StatusCode}, fmt.Errorf("llmapi: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var parsed struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return RunResult{}, fmt.Errorf("llmapi: bad response: %w", err)
	}
	var sb strings.Builder
	for _, c := range parsed.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return RunResult{FinalText: sb.String()}, nil
}
