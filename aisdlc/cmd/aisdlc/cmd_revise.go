package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"aisdlc/internal/agents"
	"aisdlc/internal/atlassian"
	"aisdlc/internal/gates"
	"aisdlc/internal/interview"
	"aisdlc/internal/orchestrator"
	"aisdlc/internal/store"
	"aisdlc/internal/tracker"
)

// cmdRevise turns captured reviewer feedback into the next PRD version. It can run any time
// after the reconciler sent the ticket to PRD_REVISION: the feedback was saved to disk when it
// arrived, so it does not matter how long ago the reviewers wrote it.
func cmdRevise(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return exitUsage, usageError{"usage: aisdlc revise KEY [agent flags]"}
	}
	key, err := normalizeKey(args[0])
	if err != nil {
		return exitUsage, err
	}
	fs := flag.NewFlagSet("revise", flag.ContinueOnError)
	af := addAgentFlags(fs)
	dataDir := fs.String("data", "", "data directory (default: data_dir from config)")
	commentsDir := fs.String("comments-dir", "", "comment folder used to post the changelog back (default: <data>/comments)")
	if err := fs.Parse(args[1:]); err != nil {
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
	st, err := openStore(cfg)
	if err != nil {
		return exitErr, err
	}
	lim := orchestrator.Limits{ReviewRounds: cfg.Limits.ReviewRounds, BuildAttempts: cfg.Limits.BuildAttempts}

	tk, err := st.Load(key)
	if errors.Is(err, store.ErrNotFound) {
		return exitErr, fmt.Errorf("no such ticket %s", key)
	}
	if err != nil {
		return exitErr, err
	}
	if tk.State != orchestrator.PRDRevision {
		return exitErr, fmt.Errorf("%s is in state %s; revise only runs from PRD_REVISION (the reconciler moves a ticket there once reviewers ask for changes)", key, tk.State)
	}
	v := tk.Ctx.PRDVersion
	exp := store.Expect{State: orchestrator.PRDRevision, PRDVersion: v}

	// Crash safety: if a previous run wrote v+1 but died before advancing the state, reuse it.
	if last, ok := tk.LatestArtifact("prd"); ok && last.Version == v+1 {
		if _, err := st.ApplyExpect(key, orchestrator.EvRevisionReady, fmt.Sprintf("PRD v%d (recovered from an interrupted revise)", last.Version), lim, exp); err != nil {
			return exitErr, err
		}
		fmt.Printf("PRD v%d already existed from an interrupted run; %s is back in PRD_REVIEW.\n", last.Version, key)
		return exitOK, nil
	}

	fb, err := st.LoadFeedback(key, "prd", v)
	if err != nil {
		return exitErr, fmt.Errorf("no saved feedback for %s PRD v%d: %w", key, v, err)
	}
	sess, err := interview.LoadSession(st, key)
	if err != nil {
		return exitErr, err
	}
	if sess == nil {
		return exitErr, fmt.Errorf("no saved interview session for %s: revise needs the original interview to rewrite the PRD", key)
	}

	adapterName := cfg.AdapterName(agents.RoleInterviewer, ov)
	if err := requireBinary(cfg, adapterName); err != nil {
		return exitErr, err
	}
	adapter, err := reg.Get(adapterName)
	if err != nil {
		return exitErr, err
	}
	base := cfg.Request(agents.RoleInterviewer, ov)
	base.Workdir = "."
	d := &interview.Driver{Adapter: auditAdapter{Adapter: adapter, st: st, key: key}, Base: base, In: bufio.NewReader(os.Stdin), Out: os.Stdout}

	// Put the whole conversation into the transcript, oldest first, labelled by who said it:
	// change requests from designated approvers, and everything else as discussion. Done once, so
	// a retry never duplicates it. All of it is third-party text, so it is wrapped as data.
	fbMarker := fmt.Sprintf("REVIEWER FEEDBACK on PRD v%d", v)
	dMarker := fmt.Sprintf("DISCUSSION on PRD v%d", v)
	already := false
	for _, t := range sess.Turns {
		if strings.HasPrefix(t.Question, fbMarker) || strings.HasPrefix(t.Question, dMarker) {
			already = true
			break
		}
	}
	if !already {
		type entry struct {
			c      gates.Comment
			change bool
		}
		var all []entry
		for _, c := range fb.Changes {
			all = append(all, entry{c, true})
		}
		for _, c := range fb.Discussion {
			all = append(all, entry{c, false})
		}
		sort.SliceStable(all, func(i, j int) bool { return all[i].c.Created.Before(all[j].c.Created) })
		for _, e := range all {
			role := fb.Roles[e.c.Author]
			if role == "" {
				role = "participant"
			}
			q := fmt.Sprintf("%s — %s (%s)", dMarker, e.c.Author, role)
			if e.change {
				q = fmt.Sprintf("%s from %s (%s) — revise the PRD to address this", fbMarker, e.c.Author, role)
			}
			sess.Turns = append(sess.Turns, interview.Turn{
				Question: q,
				Answer:   interview.WrapUntrusted("jira:"+key+" comment by "+e.c.Author, e.c.Body),
				At:       e.c.Created,
			})
		}
	}

	md, lint, missing, err := d.GeneratePRD(ctx, sess, v+1)
	if err != nil {
		_ = interview.SaveSession(st, sess)
		return exitErr, fmt.Errorf("%w (feedback is kept; rerun `aisdlc revise %s` to retry)", err, key)
	}
	art, err := st.AddArtifact(key, "prd", []byte(md))
	if err != nil {
		return exitErr, err
	}
	if err := interview.SaveSession(st, sess); err != nil {
		return exitErr, err
	}
	if _, err := st.ApplyExpect(key, orchestrator.EvRevisionReady, fmt.Sprintf("PRD v%d written from %d change request(s) and %d other comment(s) (sha256 %.12s)", art.Version, len(fb.Changes), len(fb.Discussion), art.SHA256), lim, exp); err != nil {
		return exitErr, err
	}
	fmt.Printf("PRD v%d saved; read it with: aisdlc show %s\n", art.Version, key)
	for _, f := range lint {
		fmt.Printf("  lint: line %d mentions %q — implementation detail belongs in the engineering appendix\n", f.Line, f.Term)
	}
	if len(missing) > 0 {
		fmt.Printf("  warning: sections missing from the PRD: %s\n", strings.Join(missing, "; "))
	}
	fmt.Printf("\n%s is back in PRD_REVIEW. Earlier approvals do not carry over: approvers must sign off v%d.\n", key, art.Version)

	// Tell the thread what changed, so people do not have to diff two documents.
	log := interview.Changelog(md)
	if log == "" {
		log = "(the writer did not provide a changelog; compare the two versions)"
	}
	if hub, herr := newHub(cfg, st); herr != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", herr)
	} else if hub != nil {
		if err := publishDoc(ctx, hub, st, key, "prd", art, v, log); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not attach v%d to Jira (%v); retry with: aisdlc publish %s\n", art.Version, err, key)
		} else {
			fmt.Printf("Attached to the [PRD] thread with the changelog.\n")
		}
	} else {
		cdir := *commentsDir
		if cdir == "" {
			cdir = filepath.Join(cfg.DataDir, "comments")
		}
		body := atlassian.DocNote("prd", art.Version, v, log)
		if err := (tracker.DirTracker{Dir: cdir}).Post(ctx, key, tracker.ThreadPRD, gates.Comment{
			ID: fmt.Sprintf("bot-%d", time.Now().UnixNano()), Author: cfg.BotUser, Created: time.Now().UTC(), Body: body}); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not post the changelog to the PRD thread: %v\n", err)
		}
	}
	return exitOK, nil
}
