package agents

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFakeCLI creates an executable shell script that records its argv and stdin into dir,
// then runs body.
func writeFakeCLI(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	script := "#!/bin/sh\ncat > \"" + dir + "/" + name + ".stdin\"\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > \"" + dir + "/" + name + ".args\"\n" + body + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestClaudeAdapter(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCLI(t, dir, "claude", `echo '{"result":"hello","session_id":"s1","total_cost_usd":0.02,"is_error":false}'`)
	a := NewClaude(bin)

	t.Run("read-only with tools, prompt on stdin", func(t *testing.T) {
		res, err := a.Run(context.Background(), RunRequest{Role: RoleProposer, Prompt: "THE PROMPT", SystemPrompt: "SYS", Sandbox: ReadOnly, MaxTurns: 7, Timeout: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if res.FinalText != "hello" || res.SessionID != "s1" || res.CostUSD != 0.02 {
			t.Fatalf("bad result: %+v", res)
		}
		if got := readFile(t, dir+"/claude.stdin"); got != "THE PROMPT" {
			t.Fatalf("stdin = %q", got)
		}
		args := strings.Split(strings.TrimSpace(readFile(t, dir+"/claude.args")), "\n")
		if args[0] != "-p" || args[1] != "Complete the task described on stdin." {
			t.Fatalf("positional query must directly follow -p: %v", args)
		}
		joined := strings.Join(args, "|")
		for _, want := range []string{"--output-format|json", "--append-system-prompt|SYS", "--max-turns|7", "--allowedTools|Read,Grep,Glob"} {
			if !strings.Contains(joined, want) {
				t.Errorf("missing %q in %s", want, joined)
			}
		}
	})

	t.Run("NoTools omits allowedTools", func(t *testing.T) {
		if _, err := a.Run(context.Background(), RunRequest{Prompt: "p", NoTools: true}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(readFile(t, dir+"/claude.args"), "--allowedTools") {
			t.Fatal("NoTools run must not pass --allowedTools")
		}
	})

	t.Run("write sandbox grants edit tools; schema and resume pass through", func(t *testing.T) {
		_, err := a.Run(context.Background(), RunRequest{Prompt: "p", Sandbox: WorkspaceWrite, OutputSchema: []byte(`{"type":"object"}`), ResumeID: "abc"})
		if err != nil {
			t.Fatal(err)
		}
		args := readFile(t, dir+"/claude.args")
		for _, want := range []string{"Edit", "Bash", "--json-schema", `{"type":"object"}`, "--resume", "abc"} {
			if !strings.Contains(args, want) {
				t.Errorf("missing %q", want)
			}
		}
	})

	t.Run("is_error surfaces as error", func(t *testing.T) {
		d2 := t.TempDir()
		bad := writeFakeCLI(t, d2, "claude", `echo '{"result":"boom","is_error":true}'`)
		if _, err := NewClaude(bad).Run(context.Background(), RunRequest{Prompt: "p"}); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("timeout is enforced", func(t *testing.T) {
		d3 := t.TempDir()
		slow := writeFakeCLI(t, d3, "claude", "sleep 5")
		start := time.Now()
		_, err := NewClaude(slow).Run(context.Background(), RunRequest{Prompt: "p", Timeout: 300 * time.Millisecond})
		if err == nil || time.Since(start) > 3*time.Second {
			t.Fatalf("expected quick timeout error, got %v after %v", err, time.Since(start))
		}
	})
}

func TestAgyAdapter(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeCLI(t, dir, "agy", `echo '{"response":"hi from agy","conversation_id":"c42"}'`)

	t.Run("direct mode parses tolerant JSON", func(t *testing.T) {
		a := &AgyAdapter{Bin: bin}
		res, err := a.Run(context.Background(), RunRequest{Role: RoleReviewer, Prompt: "PROMPT", SystemPrompt: "SYS"})
		if err != nil {
			t.Fatal(err)
		}
		if res.FinalText != "hi from agy" || res.SessionID != "c42" {
			t.Fatalf("bad result: %+v", res)
		}
		args := readFile(t, dir+"/agy.args")
		if !strings.HasPrefix(args, "-p\nSYS") || !strings.Contains(args, "PROMPT") || !strings.Contains(args, "--output-format\njson") {
			t.Fatalf("prompt arg wrong: %q", args)
		}
		if strings.Contains(args, "dangerously") {
			t.Fatal("must not skip permissions by default")
		}
	})

	t.Run("plain text output falls back to raw text", func(t *testing.T) {
		d := t.TempDir()
		b := writeFakeCLI(t, d, "agy", `echo 'just text'`)
		res, err := (&AgyAdapter{Bin: b}).Run(context.Background(), RunRequest{Prompt: "p"})
		if err != nil || res.FinalText != "just text" {
			t.Fatalf("got %+v, %v", res, err)
		}
	})

	t.Run("skip-permissions only for write roles when enabled", func(t *testing.T) {
		a := &AgyAdapter{Bin: bin, SkipPermissionsForWrite: true}
		if _, err := a.Run(context.Background(), RunRequest{Prompt: "p", Sandbox: WorkspaceWrite}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(readFile(t, dir+"/agy.args"), "--dangerously-skip-permissions") {
			t.Fatal("expected flag for write role")
		}
		if _, err := a.Run(context.Background(), RunRequest{Prompt: "p", Sandbox: ReadOnly}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(readFile(t, dir+"/agy.args"), "--dangerously-skip-permissions") {
			t.Fatal("read-only role must never skip permissions")
		}
	})

	t.Run("empty stdout gives actionable error", func(t *testing.T) {
		d := t.TempDir()
		b := writeFakeCLI(t, d, "agy", `true`)
		_, err := (&AgyAdapter{Bin: b}).Run(context.Background(), RunRequest{Prompt: "p"})
		if err == nil || !strings.Contains(err.Error(), "pty") {
			t.Fatalf("want pty hint, got %v", err)
		}
	})

	t.Run("oversized prompt rejected before spawning", func(t *testing.T) {
		_, err := (&AgyAdapter{Bin: bin}).Run(context.Background(), RunRequest{Prompt: strings.Repeat("x", maxAgyPromptBytes+1)})
		if err == nil || !strings.Contains(err.Error(), "argv limit") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("per-role HOME and provider settings", func(t *testing.T) {
		d := t.TempDir()
		b := writeFakeCLI(t, d, "agy", `echo "{\"response\":\"$HOME\"}"`)
		root := filepath.Join(d, "homes")
		a := &AgyAdapter{Bin: b, HomeRoot: root, Provider: "gemini"}
		res, err := a.Run(context.Background(), RunRequest{Role: RoleQA, Prompt: "p"})
		if err != nil {
			t.Fatal(err)
		}
		if res.FinalText != filepath.Join(root, "qa") {
			t.Fatalf("HOME not isolated: %q", res.FinalText)
		}
		s := readFile(t, filepath.Join(root, "qa", ".gemini", "antigravity-cli", "settings.json"))
		if !strings.Contains(s, `"modelProvider":"gemini"`) {
			t.Fatalf("settings.json = %s", s)
		}
	})

	t.Run("script pty wrapper", func(t *testing.T) {
		if _, err := exec.LookPath("script"); err != nil {
			t.Skip("script(1) not available")
		}
		a := &AgyAdapter{Bin: bin, PTY: "script"}
		res, err := a.Run(context.Background(), RunRequest{Prompt: "it's quoted"})
		if err != nil {
			t.Fatal(err)
		}
		if res.FinalText != "hi from agy" {
			t.Fatalf("got %q", res.FinalText)
		}
		if !strings.Contains(readFile(t, dir+"/agy.args"), "it's quoted") {
			t.Fatal("quoting through script(1) mangled the prompt")
		}
	})
}

func TestLLMAPIAdapter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("x-api-key") != "k123" || r.Header.Get("anthropic-version") == "" {
			http.Error(w, "bad request", 400)
			return
		}
		w.Write([]byte(`{"content":[{"type":"text","text":"part1 "},{"type":"text","text":"part2"}]}`))
	}))
	defer srv.Close()

	a := NewLLMAPI("m", "TEST_KEY_ENV")
	a.BaseURL = srv.URL
	res, err := a.Run(context.Background(), RunRequest{Prompt: "p", SystemPrompt: "s", Env: map[string]string{"TEST_KEY_ENV": "k123"}})
	if err != nil || res.FinalText != "part1 part2" {
		t.Fatalf("got %+v, %v", res, err)
	}
	if _, err := a.Run(context.Background(), RunRequest{Prompt: "p", Env: map[string]string{"TEST_KEY_ENV": "wrong"}}); err == nil {
		t.Fatal("expected HTTP error for bad key")
	}
	os.Unsetenv("TEST_KEY_ENV")
	if _, err := a.Run(context.Background(), RunRequest{Prompt: "p"}); err == nil {
		t.Fatal("expected error when key missing")
	}
}

func TestExtractJSON(t *testing.T) {
	cases := map[string]bool{
		"```json\n{\"a\":1}\n```":          true,
		"Sure! Here: {\"a\":{\"b\":2}} ok": true,
		"no json here":                     false,
		"{broken":                          false,
	}
	for in, ok := range cases {
		_, err := ExtractJSON(in)
		if (err == nil) != ok {
			t.Errorf("ExtractJSON(%q) err=%v, want ok=%v", in, err, ok)
		}
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	r.Register(NewClaude(""))
	r.Register(NewAgy(""))
	if _, err := r.Get("claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get("nope"); err == nil {
		t.Fatal("expected unknown adapter error")
	}
	if got := strings.Join(r.Names(), ","); got != "agy,claude" {
		t.Fatalf("names = %s", got)
	}
}
