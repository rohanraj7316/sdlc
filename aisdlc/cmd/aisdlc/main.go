// Command aisdlc drives the Jira-centred AI SDLC pipeline. This build covers Phase 0/1
// foundations: the PRD grill-up, the ticket state machine and store, sign-off gate checks,
// config validation and adapter contract tests. Jira/Bitbucket integration comes next.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"aisdlc/internal/agents"
	"aisdlc/internal/config"
	"aisdlc/internal/store"
)

const version = "0.1.0"

const usage = `aisdlc ` + version + ` — Jira-centred AI SDLC pipeline

Usage:
  aisdlc doctor [flags]             first contact: read-only checks of your real Jira / Bitbucket
  aisdlc intake [flags]             pick up tickets carrying the trigger label; create their threads
  aisdlc grill KEY [flags]          PRD grill-up: AI interviews you, then writes the PRD
  aisdlc publish KEY [flags]        attach the latest PRD / technical plan to the ticket's thread
  aisdlc status [KEY]               list tickets (with how long each has waited), or show one ticket
  aisdlc show KEY [prd|tech]        print a stored document (latest version, or --version N)
  aisdlc reconcile [flags]          wake tickets waiting on humans: picks up replies whenever they arrive
  aisdlc revise KEY [flags]         turn reviewer feedback into the next PRD version
  aisdlc gate-check [flags]         evaluate sign-off comments for a PRD/tech plan (exit 3 = not passed)
  aisdlc config-check [flags]       validate config and show which agent runs each role
  aisdlc adapters-test [flags]      contract-test the real claude/agy/llmapi installs
  aisdlc version

Every command that picks agents accepts per-run overrides, e.g. --builder agy --reviewer claude.
Run "aisdlc <command> -h" for flags. KEY looks like PROJ-123.
`

// exit codes
const (
	exitOK       = 0
	exitErr      = 1
	exitUsage    = 2
	exitNotReady = 3
)

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitUsage)
	}
	cmd, args := os.Args[1], os.Args[2:]
	ctx, stop := signalContext()
	defer stop()

	var code int
	var err error
	switch cmd {
	case "grill":
		code, err = cmdGrill(ctx, args)
	case "status":
		code, err = cmdStatus(args)
	case "show":
		code, err = cmdShow(args)
	case "doctor":
		code, err = cmdDoctor(ctx, args)
	case "intake":
		code, err = cmdIntake(ctx, args)
	case "publish":
		code, err = cmdPublish(ctx, args)
	case "reconcile":
		code, err = cmdReconcile(ctx, args)
	case "revise":
		code, err = cmdRevise(ctx, args)
	case "gate-check":
		code, err = cmdGateCheck(args)
	case "config-check":
		code, err = cmdConfigCheck(args)
	case "adapters-test":
		code, err = cmdAdaptersTest(ctx, args)
	case "version", "--version", "-v":
		fmt.Println("aisdlc", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(exitUsage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		var ue usageError
		if asUsage(err, &ue) {
			code = exitUsage
		} else if code == 0 {
			code = exitErr
		}
	}
	os.Exit(code)
}

func asUsage(err error, target *usageError) bool {
	ue, ok := err.(usageError)
	if ok {
		*target = ue
	}
	return ok
}

// ---- shared flag plumbing ----------------------------------------------------------------

// agentFlags are the per-role overrides plus config path, shared by several commands.
type agentFlags struct {
	configPath string
	roles      map[agents.Role]*string
}

func addAgentFlags(fs *flag.FlagSet) *agentFlags {
	af := &agentFlags{roles: map[agents.Role]*string{}}
	fs.StringVar(&af.configPath, "config", "", "path to aisdlc.json (default: built-in defaults)")
	names := map[agents.Role]string{
		agents.RoleInterviewer: "interviewer", agents.RoleProposer: "proposer",
		agents.RoleBuilder: "builder", agents.RoleReviewer: "reviewer", agents.RoleQA: "qa",
	}
	for _, r := range agents.AllRoles {
		af.roles[r] = fs.String(names[r], "", "override the adapter for "+string(r)+" (claude|agy|llmapi|fake)")
	}
	return af
}

func (af *agentFlags) overrides() config.Overrides {
	ov := config.Overrides{}
	for r, v := range af.roles {
		if *v != "" {
			ov[r] = *v
		}
	}
	return ov
}

// loadAll loads config, builds the registry (plus the offline demo adapter) and validates.
func (af *agentFlags) loadAll() (config.Config, *agents.Registry, config.Overrides, []string, error) {
	cfg, err := config.Load(af.configPath)
	if err != nil {
		return cfg, nil, nil, nil, err
	}
	reg := cfg.BuildRegistry()
	reg.Register(agents.NewDemo())
	ov := af.overrides()
	warnings, err := cfg.Validate(reg, ov)
	return cfg, reg, ov, warnings, err
}

func printWarnings(ws []string) {
	for _, w := range ws {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
}

// binaryFor returns the executable an adapter needs, or "" if it needs none.
func binaryFor(cfg config.Config, adapter string) string {
	switch adapter {
	case "claude":
		if cfg.Claude.Bin != "" {
			return cfg.Claude.Bin
		}
		return "claude"
	case "agy":
		if cfg.Agy.Bin != "" {
			return cfg.Agy.Bin
		}
		return "agy"
	}
	return ""
}

func requireBinary(cfg config.Config, adapter string) error {
	bin := binaryFor(cfg, adapter)
	if bin == "" {
		return nil
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("adapter %q needs %q on PATH (or set %s.bin in the config): %w", adapter, bin, adapter, err)
	}
	if adapter == "agy" && cfg.Agy.PTY == "script" {
		if _, err := exec.LookPath("script"); err != nil {
			return fmt.Errorf("agy.pty is \"script\" but script(1) is not installed; set agy.pty to \"\" or install util-linux")
		}
	}
	return nil
}

// auditAdapter records every agent run (never prompt content) in the ticket's event log.
type auditAdapter struct {
	agents.Adapter
	st  *store.Store
	key string
}

func (a auditAdapter) Run(ctx context.Context, req agents.RunRequest) (agents.RunResult, error) {
	start := time.Now()
	res, err := a.Adapter.Run(ctx, req)
	detail := map[string]any{
		"role": req.Role, "adapter": a.Name(), "prompt_bytes": len(req.Prompt),
		"duration_ms": time.Since(start).Milliseconds(), "cost_usd": res.CostUSD,
		"session_id": res.SessionID, "exit_code": res.ExitCode, "ok": err == nil,
	}
	if err != nil {
		detail["error"] = err.Error()
	}
	_ = a.st.LogEvent(a.key, "agent_run", detail)
	return res, err
}

// openStore connects to the central PostgreSQL database named by database.url_env.
func openStore(cfg config.Config) (*store.Store, error) {
	dsn := os.Getenv(cfg.Database.URLEnv)
	if dsn == "" {
		return nil, fmt.Errorf("environment variable %s is not set; it must hold the PostgreSQL URL, e.g. postgres://user:pass@host:5432/aisdlc", cfg.Database.URLEnv)
	}
	return store.Open(dsn)
}

func normalizeKey(s string) (string, error) {
	k := strings.ToUpper(strings.TrimSpace(s))
	if err := store.ValidKey(k); err != nil {
		return "", usageError{err.Error()}
	}
	return k, nil
}
