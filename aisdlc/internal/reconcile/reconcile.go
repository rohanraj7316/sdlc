// Package reconcile makes human delay and human back-and-forth harmless. Tickets that are
// waiting on people sit in a parked state (PRD_REVIEW, TECH_REVIEW, PR_OPEN, MERGED, QA_FAILED,
// QA_PASSED) with nothing running: no process, no agent session. A reconciler pass wakes them
// by re-deriving the truth from the tracker and, only if the situation changed, applying one
// compare-and-set transition. Because every pass starts from scratch it does not matter whether
// a reply took an hour or three weeks, whether the process restarted in between, or whether two
// reconcilers run at once.
//
// People talk freely in every thread. What moves a ticket is explicit:
//
//	approval phrase  a designated approver signs off ("approved", "okay, done", ...)
//	/changes         a designated approver / code reviewer asks for a revision / rework
//	/fix, /qa        a designated QA owner sends findings to the developer / (re)runs QA
//	/ask             anyone asks a question; the AI answers in the thread (documents only)
//
// Everything else is discussion: kept as context for the next revision, never a trigger.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"aisdlc/internal/gates"
	"aisdlc/internal/orchestrator"
	"aisdlc/internal/store"
	"aisdlc/internal/tracker"
)

type Action string

const (
	ActionPassed   Action = "passed"   // sign-off gate satisfied; ticket advanced
	ActionFeedback Action = "feedback" // changes requested; ticket sent to revision / rework / fix
	ActionAdvanced Action = "advanced" // another human decision moved it (merged, declined, QA started, QA signed off)
	ActionAnswered Action = "answered" // the AI answered one or more /ask questions
	ActionWaiting  Action = "waiting"  // nothing to do yet (the normal state of a slow review)
	ActionSettling Action = "settling" // a request exists but people are still typing
	ActionStale    Action = "stale"    // ticket moved while we were deciding; re-evaluated next pass
	ActionSkipped  Action = "skipped"  // cannot evaluate (misconfiguration / inconsistent data)
	ActionError    Action = "error"    // transient problem (tracker down); state untouched
)

type Outcome struct {
	Key     string
	Gate    string
	Action  Action
	Detail  string
	Changed bool // a state transition happened
}

// AnswerRequest is everything an Answerer needs to answer one /ask.
type AnswerRequest struct {
	Key        string
	Gate       string // "prd" or "tech"
	Document   string // the document version under review
	Discussion []gates.Comment
	Question   gates.Comment
	Role       func(author string) string
}

// Answerer produces the AI's reply to a question about a document. It must answer only from
// the document and the discussion, and must never change requirements.
type Answerer interface {
	Answer(ctx context.Context, req AnswerRequest) (string, error)
}

type Reconciler struct {
	Store   *store.Store
	Tracker tracker.Tracker
	Limits  orchestrator.Limits
	// Approvers returns who is authorized for a gate: "prd", "tech" (sign-off), "pr" (code
	// reviewers) or "qa" (QA owners) for a ticket.
	Approvers func(key, gate string) ([]string, gates.Quorum)
	// FeedbackQuiet is how long a request must sit with no further comments before the ticket
	// is sent back, so a reviewer typing several comments triggers one revision.
	FeedbackQuiet time.Duration
	Now           func() time.Time
	// Roles labels commenters (product, developer, qa...). Optional.
	Roles func(user string) string
	// Bot is the author name for the assistant's own comments.
	Bot string
	// Answerer, if set (and the tracker can Post), answers /ask questions on documents.
	Answerer   Answerer
	MaxAnswers int // per ticket per pass (default 5), to bound cost
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) role(user string) string {
	if r.Bot != "" && strings.EqualFold(user, r.Bot) {
		return "AI assistant" // the reconciler knows its own comments; never depend on config for that
	}
	if r.Roles != nil {
		return r.Roles(user)
	}
	return "participant"
}

// Tick runs one reconciliation pass over every ticket. It never returns an error: a problem
// with one ticket is reported as an Outcome and does not stop the others.
func (r *Reconciler) Tick(ctx context.Context) []Outcome {
	keys, err := r.Store.List()
	if err != nil {
		return []Outcome{{Action: ActionError, Detail: "list tickets: " + err.Error()}}
	}
	var out []Outcome
	for _, k := range keys {
		if ctx.Err() != nil {
			break
		}
		if o := r.check(ctx, k); o != nil {
			out = append(out, *o)
		}
	}
	return out
}

// Run loops Tick until ctx is cancelled, calling report after every pass.
func (r *Reconciler) Run(ctx context.Context, every time.Duration, report func([]Outcome)) {
	for {
		report(r.Tick(ctx))
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (r *Reconciler) check(ctx context.Context, key string) *Outcome {
	t, err := r.Store.Load(key)
	if err != nil {
		return &Outcome{Key: key, Action: ActionError, Detail: "load: " + err.Error()}
	}
	switch t.State {
	case orchestrator.PRDReview:
		return r.checkDoc(ctx, t, "prd", t.Ctx.PRDVersion)
	case orchestrator.TechReview:
		return r.checkDoc(ctx, t, "tech", t.Ctx.TechVersion)
	case orchestrator.PROpen:
		return r.checkPR(ctx, t)
	case orchestrator.Merged, orchestrator.QAFailed:
		return r.checkQA(ctx, t)
	case orchestrator.QAPassed:
		return r.checkQASignoff(ctx, t)
	}
	return nil // not waiting on a person
}

// ---- shared helpers ---------------------------------------------------------------------

func (r *Reconciler) fetch(ctx context.Context, key string, th tracker.Thread) ([]gates.Comment, error) {
	return r.Tracker.Comments(ctx, key, th)
}

func newest(lists ...[]gates.Comment) time.Time {
	var n time.Time
	for _, l := range lists {
		for _, c := range l {
			if c.Created.After(n) {
				n = c.Created
			}
		}
	}
	return n
}

func (r *Reconciler) feedback(changes, discussion []gates.Comment) store.Feedback {
	fb := store.Feedback{Changes: changes, Discussion: discussion, Roles: map[string]string{}}
	for _, l := range [][]gates.Comment{changes, discussion} {
		for _, c := range l {
			fb.Roles[c.Author] = r.role(c.Author)
		}
	}
	return fb
}

func (r *Reconciler) applyFailed(o *Outcome, err error) *Outcome {
	if errors.Is(err, store.ErrStale) {
		o.Action, o.Detail = ActionStale, err.Error()
	} else {
		o.Action, o.Detail = ActionError, "apply: "+err.Error()
	}
	return o
}

func uniqueAuthors(cs []gates.Comment) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cs {
		if k := strings.ToLower(c.Author); !seen[k] {
			seen[k] = true
			out = append(out, c.Author)
		}
	}
	return out
}

// withoutBotNotes drops the assistant's own housekeeping comments ("PRD v2 is attached...",
// the welcome note) from a discussion. Its answers to /ask (which carry ReplyTo) stay: they are
// part of what people read and decided on.
func (r *Reconciler) withoutBotNotes(list []gates.Comment) []gates.Comment {
	if r.Bot == "" {
		return list
	}
	out := make([]gates.Comment, 0, len(list))
	for _, c := range list {
		if strings.EqualFold(c.Author, r.Bot) && c.ReplyTo == "" {
			continue
		}
		out = append(out, c)
	}
	return out
}

// settling reports whether the conversation is still fresh (someone commented recently).
func (r *Reconciler) settling(lastComment time.Time) (time.Duration, bool) {
	age := r.now().Sub(lastComment)
	return age, age < r.FeedbackQuiet
}

// ---- PRD / technical plan sign-off ------------------------------------------------------

func (r *Reconciler) checkDoc(ctx context.Context, t *store.Ticket, gate string, version int) *Outcome {
	key := t.Key
	o := Outcome{Key: key, Gate: gate}
	skip := func(format string, a ...any) *Outcome {
		o.Action, o.Detail = ActionSkipped, fmt.Sprintf(format, a...)
		return &o
	}
	art, ok := t.LatestArtifact(gate)
	if !ok {
		return skip("state is %s but no %s artifact is recorded", t.State, gate)
	}
	if art.Version != version {
		return skip("state expects %s v%d but the latest artifact is v%d", gate, version, art.Version)
	}
	approvers, quorum := r.Approvers(key, gate)
	if len(approvers) == 0 {
		return skip("no approvers configured for the %s gate of %s", gate, key)
	}
	comments, err := r.fetch(ctx, key, tracker.Thread(gate))
	if err != nil {
		// A tracker outage is not a ticket problem: leave everything as it is and retry.
		o.Action, o.Detail = ActionError, "tracker: "+err.Error()
		return &o
	}
	res := gates.Evaluate(gates.Config{Approvers: approvers, Quorum: quorum, Version: version, UploadedAt: art.Created}, comments)
	res.Discussion = r.withoutBotNotes(res.Discussion)
	exp := store.Expect{State: t.State}
	if gate == "prd" {
		exp.PRDVersion = version
	} else {
		exp.TechVersion = version
	}
	since := t.InStateSince()
	label := strings.ToUpper(gate)

	switch {
	case res.Passed:
		names := make([]string, len(res.Approvals))
		for i, a := range res.Approvals {
			names[i] = a.Approver
		}
		note := fmt.Sprintf("%s v%d approved by %s", label, version, strings.Join(names, ", "))
		if _, err := r.Store.ApplyExpect(key, orchestrator.EvGatePassed, note, r.Limits, exp); err != nil {
			return r.applyFailed(&o, err)
		}
		_ = r.Store.LogEvent(key, "gate_passed", map[string]any{"gate": gate, "version": version, "approvals": res.Approvals})
		o.Action, o.Detail, o.Changed = ActionPassed, fmt.Sprintf("%s after waiting %s", note, Age(r.now().Sub(since))), true

	case len(res.Changes) > 0:
		last := newest(res.Changes, res.Discussion)
		if quiet, wait := r.settling(last); wait {
			// Answer any questions meanwhile, then let people finish typing.
			n, aerr := r.answerAsks(ctx, t, gate, art, comments, res)
			o.Action = ActionSettling
			o.Detail = fmt.Sprintf("changes requested on %s v%d; the thread has been quiet for %s of %s%s", gate, version, Age(quiet), Age(r.FeedbackQuiet), answeredNote(n, aerr))
			return &o
		}
		path, err := r.Store.SaveFeedback(key, gate, version, r.feedback(res.Changes, res.Discussion))
		if err != nil {
			o.Action, o.Detail = ActionError, "save feedback: "+err.Error()
			return &o
		}
		note := fmt.Sprintf("%d change request(s) from %s and %d other comment(s) on %s v%d (saved to %s)",
			len(res.Changes), strings.Join(uniqueAuthors(res.Changes), ", "), len(res.Discussion), label, version, path)
		if _, err := r.Store.ApplyExpect(key, orchestrator.EvFeedback, note, r.Limits, exp); err != nil {
			return r.applyFailed(&o, err)
		}
		_ = r.Store.LogEvent(key, "feedback", map[string]any{"gate": gate, "version": version, "changes": res.Changes, "discussion": res.Discussion})
		o.Action, o.Detail, o.Changed = ActionFeedback, note, true

	default:
		n, aerr := r.answerAsks(ctx, t, gate, art, comments, res)
		o.Detail = fmt.Sprintf("%s v%d waiting on %s for %s (%d comment(s) of discussion)%s",
			gate, version, strings.Join(res.Pending, ", "), Age(r.now().Sub(since)), len(res.Discussion), answeredNote(n, aerr))
		o.Action = ActionWaiting
		if n > 0 {
			o.Action = ActionAnswered
		}
	}
	return &o
}

func answeredNote(n int, err error) string {
	s := ""
	if n > 0 {
		s += fmt.Sprintf("; answered %d question(s)", n)
	}
	if err != nil {
		s += "; answering failed, will retry: " + err.Error()
	}
	return s
}

// answerAsks answers every unanswered /ask on a document thread (at most once each, even with
// several reconcilers running). It is optional: without an Answerer or a tracker that can
// Post, questions simply stay in the thread for humans.
func (r *Reconciler) answerAsks(ctx context.Context, t *store.Ticket, gate string, art store.Artifact, comments []gates.Comment, res gates.Result) (int, error) {
	if r.Answerer == nil || len(res.Asks) == 0 {
		return 0, nil
	}
	poster, ok := r.Tracker.(tracker.Poster)
	if !ok {
		return 0, nil
	}
	replied := map[string]bool{}
	for _, c := range comments {
		if c.ReplyTo != "" {
			replied[c.ReplyTo] = true
		}
	}
	docBytes, err := r.Store.ArtifactContent(t.Key, gate, art.Version)
	if err != nil {
		return 0, fmt.Errorf("read %s v%d: %w", gate, art.Version, err)
	}
	limit := r.MaxAnswers
	if limit <= 0 {
		limit = 5
	}
	answered := 0
	var firstErr error
	for _, q := range res.Asks {
		if replied[q.ID] || answered >= limit {
			continue
		}
		claim := gate + ":" + q.ID
		won, err := r.Store.ClaimReply(t.Key, claim)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !won {
			continue // another reconciler is on it, or it was answered already
		}
		text, err := r.Answerer.Answer(ctx, AnswerRequest{Key: t.Key, Gate: gate, Document: string(docBytes), Discussion: res.Discussion, Question: q, Role: r.role})
		if err == nil {
			body := fmt.Sprintf("AI assistant reply to %s:\n\n%s", q.Author, strings.TrimSpace(text))
			err = poster.Post(ctx, t.Key, tracker.Thread(gate), gates.Comment{
				ID: fmt.Sprintf("bot-%d", r.now().UnixNano()), Author: r.Bot, Created: r.now(), Body: body, ReplyTo: q.ID})
		}
		if err != nil {
			_ = r.Store.ReleaseReply(t.Key, claim) // retry next pass
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		_ = r.Store.LogEvent(t.Key, "ask_answered", map[string]any{"gate": gate, "question": q.ID, "by": q.Author})
		answered++
	}
	return answered, firstErr
}

// ---- human code review on the pull request ----------------------------------------------

func (r *Reconciler) checkPR(ctx context.Context, t *store.Ticket) *Outcome {
	key := t.Key
	o := Outcome{Key: key, Gate: "pr"}
	exp := store.Expect{State: orchestrator.PROpen, PRRound: t.Ctx.PRRound}

	// Merge / decline are decided in the code host, not in comments.
	if w, ok := r.Tracker.(tracker.PRWatcher); ok {
		st, err := w.PRState(ctx, key)
		if err != nil {
			o.Action, o.Detail = ActionError, "pull request state: "+err.Error()
			return &o
		}
		var ev orchestrator.Event
		switch st {
		case tracker.PRMerged:
			ev = orchestrator.EvMerged
		case tracker.PRDeclined:
			ev = orchestrator.EvPRDeclined
		}
		if ev != "" {
			note := "pull request " + string(st)
			if _, err := r.Store.ApplyExpect(key, ev, note, r.Limits, exp); err != nil {
				return r.applyFailed(&o, err)
			}
			o.Action, o.Detail, o.Changed = ActionAdvanced, note, true
			return &o
		}
	}

	reviewers, _ := r.Approvers(key, "pr")
	if len(reviewers) == 0 {
		o.Action, o.Detail = ActionWaiting, "pull request open; no code reviewers configured, so only merge/decline is tracked"
		return &o
	}
	// A ticket enters PR_OPEN when code is pushed, so anything said before that was about
	// earlier code and is history.
	since := t.InStateSince()
	comments, err := r.fetch(ctx, key, tracker.ThreadPR)
	if err != nil {
		o.Action, o.Detail = ActionError, "tracker: "+err.Error()
		return &o
	}
	round := gates.SplitRound(reviewers, since, comments)
	round.Discussion = r.withoutBotNotes(round.Discussion)
	changes := round.Commands["changes"]
	if len(changes) == 0 {
		o.Action = ActionWaiting
		o.Detail = fmt.Sprintf("pull request (human review round %d) waiting for reviewers for %s (%d comment(s) of discussion)", t.Ctx.PRRound+1, Age(r.now().Sub(t.InStateSince())), len(round.Discussion))
		return &o
	}
	if quiet, wait := r.settling(newest(changes, round.Discussion)); wait {
		o.Action = ActionSettling
		o.Detail = fmt.Sprintf("code reviewers requested changes; the thread has been quiet for %s of %s", Age(quiet), Age(r.FeedbackQuiet))
		return &o
	}
	next := t.Ctx.PRRound + 1 // the round Next() will move the ticket into
	path, err := r.Store.SaveFeedback(key, "pr", next, r.feedback(changes, round.Discussion))
	if err != nil {
		o.Action, o.Detail = ActionError, "save feedback: "+err.Error()
		return &o
	}
	note := fmt.Sprintf("%d change request(s) from %s and %d other comment(s) in human review round %d (saved to %s)",
		len(changes), strings.Join(uniqueAuthors(changes), ", "), len(round.Discussion), next, path)
	if _, err := r.Store.ApplyExpect(key, orchestrator.EvPRChanges, note, r.Limits, exp); err != nil {
		return r.applyFailed(&o, err)
	}
	_ = r.Store.LogEvent(key, "pr_changes", map[string]any{"round": next, "changes": changes, "discussion": round.Discussion})
	o.Action, o.Detail, o.Changed = ActionFeedback, note, true
	return &o
}

// ---- QA thread ---------------------------------------------------------------------------

// checkQA handles MERGED (waiting for someone to start QA) and QA_FAILED (waiting for dev and
// QA to agree: send the findings back for a fix, or retest).
func (r *Reconciler) checkQA(ctx context.Context, t *store.Ticket) *Outcome {
	key := t.Key
	o := Outcome{Key: key, Gate: "qa"}
	owners, _ := r.Approvers(key, "qa")
	if len(owners) == 0 {
		o.Action, o.Detail = ActionSkipped, fmt.Sprintf("no QA owners configured for %s, so /qa and /fix cannot be honoured", key)
		return &o
	}
	// The conversation about a QA failure (or about a fresh merge) starts when the ticket
	// entered this state; earlier comments belong to an earlier run.
	since := t.InStateSince()
	comments, err := r.fetch(ctx, key, tracker.ThreadQA)
	if err != nil {
		o.Action, o.Detail = ActionError, "tracker: "+err.Error()
		return &o
	}
	round := gates.SplitRound(owners, since, comments)
	round.Discussion = r.withoutBotNotes(round.Discussion)
	fix, run := round.Commands["fix"], round.Commands["qa"]
	if t.State == orchestrator.Merged {
		fix = nil // nothing has failed yet: only /qa means anything here
	}
	exp := store.Expect{State: t.State, QARun: t.Ctx.QARun}

	startQA := func(why string) *Outcome {
		if _, err := r.Store.ApplyExpect(key, orchestrator.EvQATrigger, why, r.Limits, exp); err != nil {
			return r.applyFailed(&o, err)
		}
		o.Action, o.Detail, o.Changed = ActionAdvanced, why, true
		return &o
	}

	switch {
	case len(fix) > 0 && (len(run) == 0 || newest(fix).After(newest(run))):
		if quiet, wait := r.settling(newest(fix, round.Discussion)); wait {
			o.Action = ActionSettling
			o.Detail = fmt.Sprintf("QA asked for a fix; the thread has been quiet for %s of %s", Age(quiet), Age(r.FeedbackQuiet))
			return &o
		}
		path, err := r.Store.SaveFeedback(key, "qa", t.Ctx.QARun, r.feedback(fix, round.Discussion))
		if err != nil {
			o.Action, o.Detail = ActionError, "save feedback: "+err.Error()
			return &o
		}
		note := fmt.Sprintf("QA run %d findings sent back to the developer by %s with %d other comment(s) (saved to %s)",
			t.Ctx.QARun, strings.Join(uniqueAuthors(fix), ", "), len(round.Discussion), path)
		if _, err := r.Store.ApplyExpect(key, orchestrator.EvQAFixRequested, note, r.Limits, exp); err != nil {
			return r.applyFailed(&o, err)
		}
		_ = r.Store.LogEvent(key, "qa_fix_requested", map[string]any{"run": t.Ctx.QARun, "fix": fix, "discussion": round.Discussion})
		o.Action, o.Detail, o.Changed = ActionFeedback, note, true
		return &o
	case len(run) > 0:
		return startQA(fmt.Sprintf("QA (re)run requested by %s", strings.Join(uniqueAuthors(run), ", ")))
	}
	o.Action = ActionWaiting
	if t.State == orchestrator.Merged {
		o.Detail = fmt.Sprintf("merged; QA starts when %s writes /qa on the QA thread (or via the CLI); waiting for %s", strings.Join(owners, " or "), Age(r.now().Sub(t.InStateSince())))
	} else {
		o.Detail = fmt.Sprintf("QA run %d failed; waiting for %s to write /fix (send to developer) or /qa (retest), for %s (%d comment(s) of discussion)",
			t.Ctx.QARun, strings.Join(owners, " or "), Age(r.now().Sub(t.InStateSince())), len(round.Discussion))
	}
	return &o
}

// checkQASignoff closes the ticket once the designated QA owners sign off a passing run.
func (r *Reconciler) checkQASignoff(ctx context.Context, t *store.Ticket) *Outcome {
	key := t.Key
	o := Outcome{Key: key, Gate: "qa"}
	owners, quorum := r.Approvers(key, "qa")
	if len(owners) == 0 {
		o.Action, o.Detail = ActionSkipped, fmt.Sprintf("no QA owners configured for %s, so nobody can sign off QA", key)
		return &o
	}
	since := t.InStateSince()
	comments, err := r.fetch(ctx, key, tracker.ThreadQA)
	if err != nil {
		o.Action, o.Detail = ActionError, "tracker: "+err.Error()
		return &o
	}
	res := gates.Evaluate(gates.Config{Approvers: owners, Quorum: quorum, Version: t.Ctx.QARun, UploadedAt: since}, comments)
	if !res.Passed {
		o.Action = ActionWaiting
		o.Detail = fmt.Sprintf("QA run %d passed; waiting for sign-off from %s for %s", t.Ctx.QARun, strings.Join(res.Pending, ", "), Age(r.now().Sub(t.InStateSince())))
		return &o
	}
	names := make([]string, len(res.Approvals))
	for i, a := range res.Approvals {
		names[i] = a.Approver
	}
	note := fmt.Sprintf("QA run %d signed off by %s", t.Ctx.QARun, strings.Join(names, ", "))
	if _, err := r.Store.ApplyExpect(key, orchestrator.EvProceed, note, r.Limits, store.Expect{State: orchestrator.QAPassed, QARun: t.Ctx.QARun}); err != nil {
		return r.applyFailed(&o, err)
	}
	o.Action, o.Detail, o.Changed = ActionAdvanced, note+"; ready for you to deploy", true
	return &o
}

// Age renders a duration the way people think about waiting: "35m", "5h 10m", "2d 4h".
func Age(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}
