package agents

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxAgyPromptBytes keeps the prompt (passed as one argv entry) under Linux's per-argument
// limit (MAX_ARG_STRLEN = 128 KiB) with headroom for the system prompt prefix.
const maxAgyPromptBytes = 100_000

// AgyAdapter drives the Antigravity CLI headless: `agy -p <prompt> --output-format json`.
//
// KNOWN UPSTREAM ISSUE: agy print mode has been reported to drop stdout (or hang) when
// stdout is not a TTY. PTY="script" wraps the call in util-linux script(1) so agy sees a
// terminal. If stdout is still empty, the adapter tries agy's on-disk transcript (best
// effort; its format is undocumented, so verify on your agy version).
type AgyAdapter struct {
	Bin                     string
	PTY                     string // "" = plain pipes, "script" = wrap in script(1)
	HomeRoot                string // if set, each role gets HOME=<HomeRoot>/<role>
	Provider                string // e.g. "gemini": writes settings.json {"modelProvider": ...}
	SkipPermissionsForWrite bool   // add --dangerously-skip-permissions for workspace_write roles only
}

func NewAgy(bin string) *AgyAdapter {
	if bin == "" {
		bin = "agy"
	}
	return &AgyAdapter{Bin: bin}
}

func (a *AgyAdapter) Name() string { return "agy" }
func (a *AgyAdapter) Capabilities() Capabilities {
	// No verified schema flag, no cost reporting. ReadOnlySandbox is true because the
	// default permission mode soft-denies writes/shell in headless runs.
	return Capabilities{StructuredOutput: false, Resume: true, ReadOnlySandbox: true, CostReporting: false}
}

func (a *AgyAdapter) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	prompt := req.Prompt
	if req.SystemPrompt != "" {
		// no verified system-prompt flag: instructions go in front of the task
		prompt = req.SystemPrompt + "\n\n---\n\n" + req.Prompt
	}
	if len(prompt) > maxAgyPromptBytes {
		return RunResult{}, fmt.Errorf("agy: prompt is %d bytes, over the %d-byte argv limit; shorten the context (e.g. summarise older interview turns)", len(prompt), maxAgyPromptBytes)
	}
	env := map[string]string{}
	for k, v := range req.Env {
		env[k] = v
	}
	if a.HomeRoot != "" {
		home := filepath.Join(a.HomeRoot, string(req.Role))
		if err := a.prepareHome(home); err != nil {
			return RunResult{}, err
		}
		env["HOME"] = home
	}
	args := []string{"-p", prompt, "--output-format", "json"}
	if a.SkipPermissionsForWrite && req.Sandbox == WorkspaceWrite {
		args = append(args, "--dangerously-skip-permissions")
	}
	if req.ResumeID != "" {
		args = append(args, "--conversation", req.ResumeID)
	}

	bin := a.Bin
	if bin == "" {
		bin = "agy"
	}
	started := time.Now()
	var res execResult
	var err error
	switch a.PTY {
	case "":
		res, err = runCmd(ctx, req.Timeout, req.Workdir, env, "", bin, args...)
	case "script":
		res, err = runViaScript(ctx, req.Timeout, req.Workdir, env, bin, args)
	default:
		return RunResult{}, fmt.Errorf("agy: unknown pty mode %q (use \"\" or \"script\")", a.PTY)
	}
	if err != nil {
		return RunResult{ExitCode: -1}, fmt.Errorf("agy: %w", err)
	}

	text := strings.TrimSpace(stripANSI(res.Stdout))
	if text == "" {
		if t, path := readLatestTranscript(env, started); t != "" {
			return RunResult{FinalText: t, ExitCode: res.ExitCode, TranscriptPath: path,
				Notes: []string{"agy: stdout was empty; recovered answer from transcript (best effort)"}}, nil
		}
		return RunResult{ExitCode: res.ExitCode}, fmt.Errorf("agy: empty stdout (exit %d). agy drops output when stdout is not a TTY: set agy.pty to \"script\". stderr: %s",
			res.ExitCode, truncate(string(res.Stderr), 300))
	}
	out := RunResult{ExitCode: res.ExitCode}
	out.FinalText, out.SessionID = parseAgyJSON(text)
	if res.ExitCode != 0 || strings.Contains(text, "AGY_ERROR") {
		return out, fmt.Errorf("agy reported failure (exit %d): %s", res.ExitCode, truncate(text, 400))
	}
	return out, nil
}

func (a *AgyAdapter) prepareHome(home string) error {
	dir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if a.Provider == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]string{"modelProvider": a.Provider})
	return os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o600)
}

// parseAgyJSON is deliberately permissive: the JSON envelope isn't verified, so look for
// common keys and otherwise return the raw text.
func parseAgyJSON(text string) (final, session string) {
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) != nil {
		return text, ""
	}
	for _, k := range []string{"result", "response", "text", "output", "content", "message"} {
		if s, ok := m[k].(string); ok && s != "" {
			final = s
			break
		}
	}
	for _, k := range []string{"conversation_id", "conversationId", "session_id", "sessionId"} {
		if s, ok := m[k].(string); ok && s != "" {
			session = s
			break
		}
	}
	if final == "" {
		final = text
	}
	return final, session
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// runViaScript wraps the command in util-linux script(1) so the child sees a pty.
func runViaScript(ctx context.Context, timeout time.Duration, dir string, env map[string]string, bin string, args []string) (execResult, error) {
	if _, err := exec.LookPath("script"); err != nil {
		return execResult{}, fmt.Errorf("pty transport needs script(1) (util-linux) in PATH: %w", err)
	}
	parts := []string{shellQuote(bin)}
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return runCmd(ctx, timeout, dir, env, "", "script", "-qefc", strings.Join(parts, " "), "/dev/null")
}

// readLatestTranscript: newest transcript.jsonl modified since `since`; returns the last
// line carrying a text-ish field. Best effort only.
func readLatestTranscript(env map[string]string, since time.Time) (text, path string) {
	home := env["HOME"]
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".gemini", "antigravity-cli", "brain", "*", ".system_generated", "logs", "transcript.jsonl"))
	type cand struct {
		path string
		mod  time.Time
	}
	var cands []cand
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.ModTime().After(since.Add(-2*time.Second)) {
			cands = append(cands, cand{m, st.ModTime()})
		}
	}
	if len(cands) == 0 {
		return "", ""
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	f, err := os.Open(cands[0].path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	for i := len(lines) - 1; i >= 0; i-- {
		var m map[string]any
		if json.Unmarshal([]byte(lines[i]), &m) != nil {
			continue
		}
		for _, k := range []string{"text", "content", "message", "response"} {
			if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
				return s, cands[0].path
			}
		}
	}
	return "", cands[0].path
}
