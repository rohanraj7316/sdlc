package agents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type execResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// runCmd runs a subprocess with a hard timeout, killing its whole process group on
// expiry. Only a minimal environment is passed through, plus env. A non-zero exit is
// reported via ExitCode (not as an error); spawn failures and timeouts are errors.
func runCmd(ctx context.Context, timeout time.Duration, dir string, env map[string]string, stdin, name string, args ...string) (execResult, error) {
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	setProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = dir
	cmd.Env = baseEnv(env)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	res := execResult{Stdout: so.Bytes(), Stderr: se.Bytes()}
	if ctx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("%s timed out after %s", name, timeout)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, err
}

func baseEnv(extra map[string]string) []string {
	keep := []string{"PATH", "HOME", "USER", "LANG", "TERM", "TMPDIR", "SHELL"}
	var out []string
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	for k, v := range extra { // later entries win, so these override the basics
		out = append(out, k+"="+v)
	}
	return out
}

var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07`)

func stripANSI(b []byte) string {
	s := ansiRe.ReplaceAllString(string(b), "")
	return strings.ReplaceAll(s, "\r", "")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
