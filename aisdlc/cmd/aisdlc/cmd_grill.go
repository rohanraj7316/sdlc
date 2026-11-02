package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"aisdlc/internal/agents"
	"aisdlc/internal/interview"
	"aisdlc/internal/orchestrator"
)

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func cmdGrill(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return exitUsage, usageError{"usage: aisdlc grill KEY [--seed-file F | --text T] [--title T] [--resume | --restart] [agent flags]"}
	}
	key, err := normalizeKey(args[0])
	if err != nil {
		return exitUsage, err
	}
	fs := flag.NewFlagSet("grill", flag.ContinueOnError)
	af := addAgentFlags(fs)
	seedFile := fs.String("seed-file", "", "file containing the ticket title/description to seed the interview")
	text := fs.String("text", "", "seed text given inline (alternative to --seed-file)")
	title := fs.String("title", "", "ticket title (used in the PRD heading)")
	resume := fs.Bool("resume", false, "continue a paused interview")
	restart := fs.Bool("restart", false, "discard any saved interview and start over")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage, usageError{err.Error()}
	}
	if *resume && *restart {
		return exitUsage, usageError{"--resume and --restart are mutually exclusive"}
	}

	cfg, reg, ov, warnings, err := af.loadAll()
	if err != nil {
		return exitErr, err
	}
	printWarnings(warnings)
	adapterName := cfg.AdapterName(agents.RoleInterviewer, ov)
	if err := requireBinary(cfg, adapterName); err != nil {
		return exitErr, err
	}
	adapter, err := reg.Get(adapterName)
	if err != nil {
		return exitErr, err
	}

	st, err := openStore(cfg)
	if err != nil {
		return exitErr, err
	}
	lim := orchestrator.Limits{ReviewRounds: cfg.Limits.ReviewRounds, BuildAttempts: cfg.Limits.BuildAttempts}
	hub, err := newHub(cfg, st)
	if err != nil {
		return exitErr, err
	}

	// Only NEW and PRD_INTERVIEW tickets may be (re)interviewed.
	tk, err := st.LoadOrCreate(key)
	if err != nil {
		return exitErr, err
	}
	if tk.State != orchestrator.New && tk.State != orchestrator.PRDInterview {
		return exitErr, fmt.Errorf("%s is in state %s; grill only runs from NEW or PRD_INTERVIEW (see: aisdlc status %s)", key, tk.State, key)
	}

	// Session: resume, restart, or start new.
	saved, err := interview.LoadSession(st, key)
	if err != nil {
		return exitErr, err
	}
	var sess *interview.Session
	switch {
	case saved != nil && *resume:
		sess = saved
		fmt.Printf("Resuming %s: %d question(s) answered so far.\n", key, len(sess.Turns))
	case saved != nil && !*restart:
		return exitErr, fmt.Errorf("a saved interview exists for %s; use --resume to continue or --restart to discard it", key)
	default:
		if *resume {
			return exitErr, fmt.Errorf("nothing to resume for %s", key)
		}
		var seed string
		if *seedFile == "" && *text == "" && hub != nil {
			// no text given: seed from the Jira ticket itself
			t, sd, err := hub.Seed(ctx, key)
			if err != nil {
				return exitErr, fmt.Errorf("reading %s from Jira: %w", key, err)
			}
			seed = sd
			if *title == "" {
				*title = t
			}
			fmt.Printf("Seeded the interview from the Jira ticket %s.\n", key)
		} else if seed, err = readSeed(*seedFile, *text); err != nil {
			return exitErr, err
		}
		sess = interview.NewSession(key, *title, seed)
	}
	if *title != "" {
		sess.Title = *title
	}

	// Move NEW -> PRD_INTERVIEW exactly once.
	if tk.State == orchestrator.New {
		if _, err := st.Apply(key, orchestrator.EvStart, "grill started with adapter "+adapterName, lim); err != nil {
			return exitErr, err
		}
	}

	if ov := af.overrides(); len(ov) > 0 {
		fmt.Fprintf(os.Stderr, "note: adapter overrides active for this run: %v\n", ov)
	}
	fmt.Printf("Interviewer: %s. Enter = accept the suggested answer; /skip /ledger /done /quit.\n", adapterName)

	in := bufio.NewReader(os.Stdin)
	base := cfg.Request(agents.RoleInterviewer, ov)
	base.Workdir = "."
	d := &interview.Driver{
		Adapter:      auditAdapter{Adapter: adapter, st: st, key: key},
		Base:         base,
		In:           in,
		Out:          os.Stdout,
		Save:         func(s *interview.Session) error { return interview.SaveSession(st, s) },
		MaxQuestions: cfg.Limits.MaxQuestions,
	}

	if err := d.Run(ctx, sess); err != nil {
		if errors.Is(err, interview.ErrQuit) {
			fmt.Printf("\nPaused. Resume with: aisdlc grill %s --resume\n", key)
			return exitOK, nil
		}
		if ctx.Err() != nil {
			_ = interview.SaveSession(st, sess)
			fmt.Printf("\nInterrupted; progress saved. Resume with: aisdlc grill %s --resume\n", key)
			return exitOK, nil
		}
		return exitErr, err
	}

	// Read-back before writing the PRD.
	fmt.Printf("\nInterview finished after %d question(s).\n", len(sess.Turns))
	printLedger(sess)
	if open := sess.OpenCritical(); len(open) > 0 {
		fmt.Printf("WARNING: %d critical area(s) are still open; they will be listed under Open questions.\n", len(open))
	}
	if !confirm(in, "Generate the PRD now?") {
		fmt.Printf("Not generated. Resume with: aisdlc grill %s --resume\n", key)
		return exitOK, nil
	}

	version := 1
	if last, ok := tk.LatestArtifact("prd"); ok {
		version = last.Version + 1
	}
	md, lint, missing, err := d.GeneratePRD(ctx, sess, version)
	if err != nil {
		_ = interview.SaveSession(st, sess)
		return exitErr, fmt.Errorf("%w (the interview is saved; rerun with --resume to retry PRD generation)", err)
	}
	art, err := st.AddArtifact(key, "prd", []byte(md))
	if err != nil {
		return exitErr, err
	}
	if err := interview.SaveSession(st, sess); err != nil {
		return exitErr, err
	}
	if _, err := st.Apply(key, orchestrator.EvInterviewDone, fmt.Sprintf("PRD v%d generated (sha256 %.12s)", art.Version, art.SHA256), lim); err != nil {
		return exitErr, err
	}

	fmt.Printf("\nPRD v%d saved; read it with: aisdlc show %s\n", art.Version, key)
	if hub != nil {
		if err := publishDoc(ctx, hub, st, key, "prd", art, 0, ""); err != nil {
			fmt.Fprintf(os.Stderr, "warning: the PRD could not be attached to Jira (%v); retry with: aisdlc publish %s\n", err, key)
		} else {
			fmt.Printf("Attached to the [PRD] thread of %s with instructions for the approvers.\n", key)
		}
	}
	for _, f := range lint {
		fmt.Printf("  lint: line %d mentions %q — implementation detail belongs in the engineering appendix\n", f.Line, f.Term)
	}
	if len(missing) > 0 {
		fmt.Printf("  warning: sections missing from the PRD: %s\n", strings.Join(missing, "; "))
	}
	for _, n := range sess.SecurityNotes {
		fmt.Printf("  security note from the interviewer: %s\n", n)
	}
	fmt.Printf("\nTicket %s is now PRD_REVIEW. Nothing is running and nothing needs to stay running: approvers can\n"+
		"take minutes or weeks. When they reply, `aisdlc reconcile` (run it once, or leave it running) picks the\n"+
		"replies up, advances the ticket on approval, or captures feedback for `aisdlc revise %s`.\n", key, key)
	return exitOK, nil
}

func readSeed(file, text string) (string, error) {
	switch {
	case file != "" && text != "":
		return "", usageError{"use either --seed-file or --text, not both"}
	case file != "":
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	case text != "":
		return strings.TrimSpace(text), nil
	}
	return "", usageError{"a new interview needs a seed: pass --seed-file FILE or --text \"...\" (or configure jira.base_url to seed from the ticket)"}
}

func printLedger(s *interview.Session) {
	fmt.Println("\nCoverage ledger:")
	for _, it := range s.Ledger {
		fmt.Printf("  [%-12s] %-9s %s\n", it.Status, it.Severity, it.Area)
	}
	fmt.Println()
}

// confirm asks a yes/no question, defaulting to yes on Enter or end of input.
func confirm(in *bufio.Reader, q string) bool {
	fmt.Printf("%s [Y/n] ", q)
	line, err := in.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "", "y", "yes":
		return true
	}
	return false
}
