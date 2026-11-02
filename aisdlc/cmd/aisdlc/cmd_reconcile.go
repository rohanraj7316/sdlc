package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"aisdlc/internal/agents"
	"aisdlc/internal/orchestrator"
	"aisdlc/internal/reconcile"
	"aisdlc/internal/tracker"
)

// cmdReconcile wakes tickets that are parked waiting for humans. Run it once from cron, or
// leave it running; either way it is safe to stop, restart, or run several of at once.
func cmdReconcile(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	af := addAgentFlags(fs)
	dataDir := fs.String("data", "", "data directory (default: data_dir from config)")
	commentsDir := fs.String("comments-dir", "", "folder with <KEY>/{prd,tech,pr,qa}.json comment files and <KEY>/pr_state (default: <data>/comments)")
	once := fs.Bool("once", false, "run a single pass and exit")
	every := fs.Duration("every", time.Minute, "pause between passes when running continuously")
	quiet := fs.Duration("feedback-quiet", 10*time.Minute, "a request for changes must sit this long with no new comments before the ticket is sent back")
	files := fs.Bool("files", false, "read conversations from the comment folder even if Jira is configured")
	noAnswer := fs.Bool("no-answer", false, "do not answer /ask questions with the AI")
	verbose := fs.Bool("v", false, "also print tickets that are simply still waiting on every pass")
	if err := fs.Parse(args); err != nil {
		return exitUsage, usageError{err.Error()}
	}
	cfg, reg, ov, warnings, err := af.loadAll()
	if err != nil {
		return exitErr, err
	}
	printWarnings(warnings)
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	cdir := *commentsDir
	if cdir == "" {
		cdir = filepath.Join(cfg.DataDir, "comments")
	}
	st, err := openStore(cfg)
	if err != nil {
		return exitErr, err
	}
	var trk tracker.Tracker = tracker.DirTracker{Dir: cdir}
	source := "comment files in " + cdir
	bot := cfg.BotUser
	if !*files {
		hub, err := newHub(cfg, st)
		if err != nil {
			return exitErr, err
		}
		if hub != nil {
			trk, source, bot = hub, "Jira "+cfg.Jira.BaseURL, botName(ctx, cfg, hub)
		}
	}
	rec := &reconcile.Reconciler{
		Store:         st,
		Tracker:       trk,
		Limits:        orchestrator.Limits{ReviewRounds: cfg.Limits.ReviewRounds, BuildAttempts: cfg.Limits.BuildAttempts},
		Approvers:     cfg.ApproversFor,
		FeedbackQuiet: *quiet,
		Roles:         cfg.RoleOf,
		Bot:           bot,
	}

	// AI answers for /ask are optional: if the agent isn't installed, gates still work.
	if !*noAnswer {
		ans := reconcile.AgentAnswerer{Adapters: map[string]agents.Adapter{}, Base: map[string]agents.RunRequest{}}
		for gate, role := range map[string]agents.Role{"prd": agents.RoleInterviewer, "tech": agents.RoleProposer} {
			name := cfg.AdapterName(role, ov)
			if err := requireBinary(cfg, name); err != nil {
				fmt.Fprintf(os.Stderr, "note: /ask on the %s thread will not be answered by the AI: %v\n", gate, err)
				continue
			}
			ad, err := reg.Get(name)
			if err != nil {
				return exitErr, err
			}
			base := cfg.Request(role, ov)
			base.Workdir = "."
			ans.Adapters[gate], ans.Base[gate] = ad, base
		}
		if len(ans.Adapters) > 0 {
			rec.Answerer = ans
		}
	}

	// Waiting is the normal state of a slow review, so only print it when asked (-v) or on
	// a one-shot pass; changes and problems are always printed.
	seenWaiting := map[string]string{}
	report := func(outs []reconcile.Outcome) {
		for _, o := range outs {
			line := fmt.Sprintf("%s  %-8s %s  %s", time.Now().Format("01-02 15:04:05"), o.Action, o.Key, o.Detail)
			switch o.Action {
			case reconcile.ActionWaiting, reconcile.ActionSettling:
				if *once || *verbose || seenWaiting[o.Key] == "" {
					fmt.Println(line)
				}
				seenWaiting[o.Key] = o.Detail
			case reconcile.ActionPassed, reconcile.ActionFeedback, reconcile.ActionAdvanced, reconcile.ActionAnswered:
				delete(seenWaiting, o.Key)
				fmt.Println(line)
			default:
				fmt.Fprintln(os.Stderr, line)
			}
		}
	}
	if *once {
		report(rec.Tick(ctx))
		return exitOK, nil
	}
	fmt.Printf("Reconciling every %s (conversations from %s). Ctrl-C to stop; it is safe to restart.\n", *every, source)
	rec.Run(ctx, *every, report)
	return exitOK, nil
}
