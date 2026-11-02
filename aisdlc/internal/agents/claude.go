package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ClaudeAdapter drives Claude Code headless: `claude -p ... --output-format json`.
// The real prompt goes on stdin (no argv size limit); the positional query is a stub.
type ClaudeAdapter struct {
	Bin string
}

func NewClaude(bin string) *ClaudeAdapter {
	if bin == "" {
		bin = "claude"
	}
	return &ClaudeAdapter{Bin: bin}
}

func (a *ClaudeAdapter) Name() string { return "claude" }
func (a *ClaudeAdapter) Capabilities() Capabilities {
	return Capabilities{StructuredOutput: true, Resume: true, ReadOnlySandbox: true, CostReporting: true}
}

func (a *ClaudeAdapter) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	res, err := runCmd(ctx, req.Timeout, req.Workdir, req.Env, req.Prompt, a.bin(), buildClaudeArgs(req)...)
	if err != nil {
		return RunResult{ExitCode: -1}, fmt.Errorf("claude: %w", err)
	}
	return parseClaudeOutput(res)
}

func (a *ClaudeAdapter) bin() string {
	if a.Bin == "" {
		return "claude"
	}
	return a.Bin
}

func buildClaudeArgs(req RunRequest) []string {
	args := []string{"-p", "Complete the task described on stdin.", "--output-format", "json"}
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}
	if req.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(req.MaxTurns))
	}
	if !req.NoTools {
		args = append(args, "--allowedTools", claudeTools(req))
	}
	if len(req.OutputSchema) > 0 {
		args = append(args, "--json-schema", string(req.OutputSchema))
	}
	if req.ResumeID != "" {
		args = append(args, "--resume", req.ResumeID)
	}
	return args
}

// claudeTools maps the abstract sandbox to a Claude Code allowlist. Unset means read-only:
// write/shell access is never granted implicitly.
func claudeTools(req RunRequest) string {
	tools := []string{"Read", "Grep", "Glob"}
	if req.Sandbox == WorkspaceWrite {
		tools = append(tools, "Edit", "Write", "Bash")
	}
	tools = append(tools, req.AllowedTools...)
	return strings.Join(tools, ",")
}

func parseClaudeOutput(res execResult) (RunResult, error) {
	out := RunResult{ExitCode: res.ExitCode}
	var j struct {
		Result           string          `json:"result"`
		SessionID        string          `json:"session_id"`
		TotalCostUSD     float64         `json:"total_cost_usd"`
		IsError          bool            `json:"is_error"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(res.Stdout, &j); err != nil {
		return out, fmt.Errorf("claude: unparseable output (exit %d): %s", res.ExitCode, truncate(string(res.Stderr)+string(res.Stdout), 400))
	}
	out.FinalText, out.SessionID, out.CostUSD = j.Result, j.SessionID, j.TotalCostUSD
	if len(j.StructuredOutput) > 0 && string(j.StructuredOutput) != "null" {
		out.Structured = j.StructuredOutput
	}
	if j.IsError || res.ExitCode != 0 {
		return out, fmt.Errorf("claude reported failure (exit %d): %s", res.ExitCode, truncate(j.Result, 400))
	}
	return out, nil
}
