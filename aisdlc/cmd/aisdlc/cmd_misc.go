package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"aisdlc/internal/agents"
	"aisdlc/internal/config"
	"aisdlc/internal/gates"
	"aisdlc/internal/orchestrator"
	"aisdlc/internal/reconcile"
	"aisdlc/internal/store"
)

// ---- status -----------------------------------------------------------------------------

func cmdStatus(args []string) (int, error) {
	key := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		k, err := normalizeKey(args[0])
		if err != nil {
			return exitUsage, err
		}
		key, args = k, args[1:]
	}
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to aisdlc.json")
	if err := fs.Parse(args); err != nil {
		return exitUsage, usageError{err.Error()}
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return exitErr, err
	}
	st, err := openStore(cfg)
	if err != nil {
		return exitErr, err
	}

	if key == "" {
		keys, err := st.List()
		if err != nil {
			return exitErr, err
		}
		if len(keys) == 0 {
			fmt.Println("No tickets yet. Start one with: aisdlc grill PROJ-123 --seed-file ticket.txt")
			return exitOK, nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "TICKET\tSTATE\tIN STATE\tUPDATED")
		for _, k := range keys {
			t, err := st.Load(k)
			if err != nil {
				fmt.Fprintf(tw, "%s\t(unreadable: %v)\t\n", k, err)
				continue
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", k, t.State, reconcile.Age(time.Since(t.InStateSince())), t.Updated.Local().Format("2006-01-02 15:04"))
		}
		tw.Flush()
		return exitOK, nil
	}

	t, err := st.Load(key)
	if errors.Is(err, store.ErrNotFound) {
		return exitErr, fmt.Errorf("no such ticket %s", key)
	}
	if err != nil {
		return exitErr, err
	}
	lim := orchestrator.Limits{ReviewRounds: cfg.Limits.ReviewRounds, BuildAttempts: cfg.Limits.BuildAttempts}
	fmt.Printf("%s  %s\n", t.Key, t.State)
	if t.State == orchestrator.Escalated {
		fmt.Printf("  escalated from %s: %s\n", t.Ctx.EscalatedFrom, t.Ctx.EscalationReason)
	}
	if what := waitingOn(t.State); what != "" {
		fmt.Printf("  %s since %s (%s). Nothing is running; replies are picked up by `aisdlc reconcile`, however long that takes.\n",
			what, t.InStateSince().Local().Format("2006-01-02 15:04"), reconcile.Age(time.Since(t.InStateSince())))
	}
	fmt.Printf("  PRD v%d · TECH v%d · build attempts %d/%d · review round %d/%d\n",
		t.Ctx.PRDVersion, t.Ctx.TechVersion, t.Ctx.BuildAttempts, lim.BuildAttempts, t.Ctx.ReviewRound, lim.ReviewRounds)
	allowed := orchestrator.Allowed(t.State, t.Ctx, lim)
	names := make([]string, len(allowed))
	for i, e := range allowed {
		names[i] = string(e)
	}
	fmt.Printf("  next events: %s\n", strings.Join(names, ", "))
	for _, a := range t.Artifacts {
		fmt.Printf("  artifact: %s v%d  (sha256 %.12s)\n", a.Kind, a.Version, a.SHA256)
	}
	start := 0
	if len(t.History) > 5 {
		start = len(t.History) - 5
	}
	for _, h := range t.History[start:] {
		note := ""
		if h.Note != "" {
			note = " — " + h.Note
		}
		fmt.Printf("  %s  %s → %s (%s)%s\n", h.At.Local().Format("01-02 15:04"), h.From, h.To, h.Event, note)
	}
	return exitOK, nil
}

// ---- show -------------------------------------------------------------------------------

// cmdShow prints a stored document. Documents live in the central database, not on disk.
func cmdShow(args []string) (int, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return exitUsage, usageError{"usage: aisdlc show KEY [prd|tech] [--version N] [--config F]"}
	}
	key, err := normalizeKey(args[0])
	if err != nil {
		return exitUsage, err
	}
	args, kind := args[1:], "prd"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		kind, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to aisdlc.json")
	ver := fs.Int("version", 0, "document version (default: latest)")
	if err := fs.Parse(args); err != nil {
		return exitUsage, usageError{err.Error()}
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return exitErr, err
	}
	st, err := openStore(cfg)
	if err != nil {
		return exitErr, err
	}
	v := *ver
	if v == 0 {
		t, err := st.Load(key)
		if err != nil {
			return exitErr, err
		}
		a, ok := t.LatestArtifact(kind)
		if !ok {
			return exitErr, fmt.Errorf("%s has no %s document", key, kind)
		}
		v = a.Version
	}
	b, err := st.ArtifactContent(key, kind, v)
	if errors.Is(err, store.ErrNotFound) {
		return exitErr, fmt.Errorf("%s has no %s v%d", key, kind, v)
	}
	if err != nil {
		return exitErr, err
	}
	os.Stdout.Write(b)
	return exitOK, nil
}

// ---- gate-check -------------------------------------------------------------------------

func cmdGateCheck(args []string) (int, error) {
	fs := flag.NewFlagSet("gate-check", flag.ContinueOnError)
	comments := fs.String("comments", "", "JSON file: [{\"id\",\"author\",\"created\",\"body\"}, ...]")
	approvers := fs.String("approvers", "", "comma-separated approver ids (accountId on Cloud, username on Data Center)")
	quorum := fs.String("quorum", "all", "all | any")
	ver := fs.Int("version", 1, "current artifact version")
	uploaded := fs.String("uploaded", "", "when that version was attached (RFC3339, e.g. 2026-10-03T10:00:00Z)")
	if err := fs.Parse(args); err != nil {
		return exitUsage, usageError{err.Error()}
	}
	if *comments == "" || *approvers == "" || *uploaded == "" {
		return exitUsage, usageError{"gate-check needs --comments, --approvers and --uploaded"}
	}
	if *quorum != "all" && *quorum != "any" {
		return exitUsage, usageError{"--quorum must be all or any"}
	}
	up, err := time.Parse(time.RFC3339, *uploaded)
	if err != nil {
		return exitUsage, usageError{"--uploaded: " + err.Error()}
	}
	b, err := os.ReadFile(*comments)
	if err != nil {
		return exitErr, err
	}
	var cs []gates.Comment
	if err := json.Unmarshal(b, &cs); err != nil {
		return exitErr, fmt.Errorf("%s: %w", *comments, err)
	}
	var list []string
	for _, a := range strings.Split(*approvers, ",") {
		if a = strings.TrimSpace(a); a != "" {
			list = append(list, a)
		}
	}
	r := gates.Evaluate(gates.Config{Approvers: list, Quorum: gates.Quorum(*quorum), Version: *ver, UploadedAt: up}, cs)

	for _, a := range r.Approvals {
		fmt.Printf("  approved   %-20s comment %s at %s\n", a.Approver, a.CommentID, a.At.Format(time.RFC3339))
	}
	for _, p := range r.Pending {
		fmt.Printf("  pending    %s\n", p)
	}
	for _, c := range r.Changes {
		fmt.Printf("  changes    %-20s comment %s: %s\n", c.Author, c.ID, oneLine(c.Body, 70))
	}
	if n := len(r.Discussion); n > 0 {
		fmt.Printf("  discussion %d other comment(s), kept as context (they never block or trigger anything)\n", n)
	}
	for _, c := range r.Stale {
		fmt.Printf("  ignored    %-20s comment %s (older version or before the upload)\n", c.Author, c.ID)
	}
	if r.Passed {
		fmt.Printf("GATE PASSED (v%d, quorum %s)\n", *ver, *quorum)
		return exitOK, nil
	}
	fmt.Printf("GATE NOT PASSED (v%d, quorum %s)\n", *ver, *quorum)
	return exitNotReady, nil
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

// ---- config-check -----------------------------------------------------------------------

func cmdConfigCheck(args []string) (int, error) {
	fs := flag.NewFlagSet("config-check", flag.ContinueOnError)
	af := addAgentFlags(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage, usageError{err.Error()}
	}
	cfg, _, ov, warnings, err := af.loadAll()
	if err != nil {
		return exitErr, err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tADAPTER\tSANDBOX\tTIMEOUT\tINSTALLED")
	for _, r := range agents.AllRoles {
		rc := cfg.Role(r, ov)
		inst := "n/a"
		if bin := binaryFor(cfg, rc.Adapter); bin != "" {
			if _, err := exec.LookPath(bin); err != nil {
				inst = "NOT FOUND (" + bin + ")"
			} else {
				inst = "yes"
			}
		}
		src := ""
		if _, ok := ov[r]; ok {
			src = " (override)"
		}
		fmt.Fprintf(tw, "%s\t%s%s\t%s\t%s\t%s\n", r, rc.Adapter, src, rc.Sandbox, rc.Timeout.Std(), inst)
	}
	tw.Flush()
	printWarnings(warnings)
	if len(warnings) == 0 {
		fmt.Println("\nIndependence rules satisfied.")
	}
	return exitOK, nil
}

// ---- adapters-test ----------------------------------------------------------------------

// cmdAdaptersTest is the "30-minute spike" as a command: it runs tiny real requests through
// each adapter so you can see exactly what your installed CLIs return.
func cmdAdaptersTest(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("adapters-test", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to aisdlc.json")
	which := fs.String("adapter", "claude,agy", "comma-separated adapters to test (claude, agy, llmapi)")
	timeout := fs.Duration("timeout", 2*time.Minute, "per-request timeout")
	verbose := fs.Bool("v", false, "print the raw text each adapter returned")
	if err := fs.Parse(args); err != nil {
		return exitUsage, usageError{err.Error()}
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return exitErr, err
	}
	reg := cfg.BuildRegistry()

	failed := 0
	for _, name := range strings.Split(*which, ",") {
		name = strings.TrimSpace(name)
		fmt.Printf("== %s ==\n", name)
		if err := requireBinary(cfg, name); err != nil {
			fmt.Printf("  SKIP  %v\n", err)
			failed++
			continue
		}
		a, err := reg.Get(name)
		if err != nil {
			fmt.Printf("  FAIL  %v\n", err)
			failed++
			continue
		}
		base := agents.RunRequest{Role: agents.RoleInterviewer, Workdir: ".", NoTools: true, Timeout: *timeout, MaxTurns: 3}

		// 1. plain text round-trip
		r1 := base
		r1.Prompt = "Reply with exactly the single word: OK"
		start := time.Now()
		res, err := a.Run(ctx, r1)
		ok1 := err == nil && strings.Contains(strings.ToUpper(res.FinalText), "OK")
		report("text round-trip", ok1, time.Since(start), res, err, *verbose)

		// 2. JSON object round-trip (what the interviewer depends on)
		r2 := base
		r2.Prompt = `Return only this JSON object and nothing else: {"answer":"pong"}`
		r2.OutputSchema = json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)
		start = time.Now()
		res, err = a.Run(ctx, r2)
		ok2 := false
		if err == nil {
			raw := res.Structured
			if len(raw) == 0 || string(raw) == "null" {
				raw, _ = agents.ExtractJSON(res.FinalText)
			}
			var v struct {
				Answer string `json:"answer"`
			}
			ok2 = json.Unmarshal(raw, &v) == nil && v.Answer == "pong"
		}
		report("JSON round-trip", ok2, time.Since(start), res, err, *verbose)

		if !ok1 || !ok2 {
			failed++
			if name == "agy" {
				fmt.Println("  hint: if stdout was empty or the call hung, set \"agy\": {\"pty\": \"script\"} in the config.")
				fmt.Println("        If the JSON shape differed, send me the raw output (-v) and I'll adjust parseAgyOutput.")
			}
		}
	}
	if failed > 0 {
		fmt.Printf("\n%d adapter(s) did not pass.\n", failed)
		return exitErr, nil
	}
	fmt.Println("\nAll adapters passed.")
	return exitOK, nil
}

func report(name string, ok bool, d time.Duration, res agents.RunResult, err error, verbose bool) {
	status := "PASS"
	if !ok {
		status = "FAIL"
	}
	fmt.Printf("  %s  %-16s %6dms", status, name, d.Milliseconds())
	if res.SessionID != "" {
		fmt.Printf("  session=%s", res.SessionID)
	}
	if res.CostUSD > 0 {
		fmt.Printf("  cost=$%.4f", res.CostUSD)
	}
	fmt.Println()
	if err != nil {
		fmt.Printf("        error: %v\n", err)
	}
	if verbose || (!ok && err == nil) {
		fmt.Printf("        raw: %s\n", oneLine(res.FinalText, 200))
	}
}

// waitingOn describes states where the pipeline is parked on a person.
func waitingOn(s orchestrator.State) string {
	switch s {
	case orchestrator.PRDReview:
		return "waiting for PRD sign-off"
	case orchestrator.TechReview:
		return "waiting for technical plan sign-off"
	case orchestrator.PRDRevision:
		return "waiting for `aisdlc revise` (reviewer feedback is saved)"
	case orchestrator.PROpen:
		return "waiting for human code review"
	case orchestrator.PRRework:
		return "waiting for the builder to address the code reviewers' requested changes"
	case orchestrator.Merged:
		return "merged; waiting for QA to be started (/qa)"
	case orchestrator.QAFailed:
		return "waiting for QA and the developer to agree: /fix or /qa"
	case orchestrator.QAPassed:
		return "waiting for QA sign-off"
	case orchestrator.Escalated:
		return "waiting for a human decision"
	}
	return ""
}
