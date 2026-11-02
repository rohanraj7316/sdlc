package reconcile

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"aisdlc/internal/gates"
	"aisdlc/internal/orchestrator"
	"aisdlc/internal/store"
	"aisdlc/internal/store/storetest"
	"aisdlc/internal/tracker"
)

var lim = orchestrator.Limits{ReviewRounds: 2, BuildAttempts: 2}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeTracker serves comments from memory; hook (if set) runs once, inside Comments, which
// lets a test simulate a human acting while the reconciler is mid-decision.
type fakeTracker struct {
	mu       sync.Mutex
	comments map[string][]gates.Comment
	err      error
	hook     func()
	pr       tracker.PRState // "" means open
	postErr  error
	posts    int
}

func (f *fakeTracker) Post(_ context.Context, key string, th tracker.Thread, c gates.Comment) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postErr != nil {
		return f.postErr
	}
	if f.comments == nil {
		f.comments = map[string][]gates.Comment{}
	}
	f.comments[key+"/"+string(th)] = append(f.comments[key+"/"+string(th)], c)
	f.posts++
	return nil
}

func (f *fakeTracker) PRState(context.Context, string) (tracker.PRState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pr == "" {
		return tracker.PROpen, nil
	}
	return f.pr, nil
}

func (f *fakeTracker) add(key, th string, c gates.Comment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.comments == nil {
		f.comments = map[string][]gates.Comment{}
	}
	f.comments[key+"/"+th] = append(f.comments[key+"/"+th], c)
}

func (f *fakeTracker) Comments(_ context.Context, key string, th tracker.Thread) ([]gates.Comment, error) {
	f.mu.Lock()
	hook := f.hook
	f.hook = nil
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return append([]gates.Comment(nil), f.comments[key+"/"+string(th)]...), nil
}

type env struct {
	t   *testing.T
	dsn string
	clk *clock
	st  *store.Store
	trk *fakeTracker
	rec *Reconciler
}

func approvers(_ string, gate string) ([]string, gates.Quorum) {
	switch gate {
	case "prd":
		return []string{"jdoe", "asmith"}, gates.QuorumAll
	case "tech":
		return []string{"dev1"}, gates.QuorumAny
	case "pr":
		return []string{"rev1", "rev2"}, gates.QuorumAny
	case "qa":
		return []string{"qa1"}, gates.QuorumAll
	}
	return nil, gates.QuorumAll
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, dsn: storetest.DSN(t), clk: &clock{t: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)}, trk: &fakeTracker{}}
	e.st = e.open(e.dsn)
	e.rec = &Reconciler{Store: e.st, Tracker: e.trk, Limits: lim, Approvers: approvers, Now: e.clk.Now}
	return e
}

func (e *env) open(dsn string) *store.Store {
	st, err := store.OpenWithClock(dsn, e.clk.Now)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { st.Close() })
	return st
}

// toReview puts a ticket into PRD_REVIEW with PRD v1 attached "now".
func (e *env) toReview(key string) {
	e.t.Helper()
	if _, err := e.st.Apply(key, orchestrator.EvStart, "", lim); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.AddArtifact(key, "prd", []byte("# PRD v1")); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.Apply(key, orchestrator.EvInterviewDone, "", lim); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) comment(key, author, body string) {
	e.trk.add(key, "prd", gates.Comment{ID: author + e.clk.Now().Format("150405.000"), Author: author, Created: e.clk.Now(), Body: body})
}

func (e *env) state(key string) orchestrator.State {
	e.t.Helper()
	tk, err := e.st.Load(key)
	if err != nil {
		e.t.Fatal(err)
	}
	return tk.State
}

func only(t *testing.T, os []Outcome) Outcome {
	t.Helper()
	if len(os) != 1 {
		t.Fatalf("want exactly 1 outcome, got %d: %+v", len(os), os)
	}
	return os[0]
}

func TestApproverAnswersDaysLater(t *testing.T) {
	e := newEnv(t)
	e.toReview("PROJ-1")

	e.clk.Advance(time.Hour)
	o := only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionWaiting || !strings.Contains(o.Detail, "jdoe, asmith") {
		t.Fatalf("after 1h: %+v", o)
	}

	e.clk.Advance(72 * time.Hour) // three days of silence cost nothing
	for i := 0; i < 3; i++ {
		if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionWaiting || o.Changed {
			t.Fatalf("still waiting expected: %+v", o)
		}
	}

	e.comment("PROJ-1", "jdoe", "okay, done")
	e.clk.Advance(10 * time.Hour)
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionWaiting || !strings.Contains(o.Detail, "asmith") || strings.Contains(o.Detail, "jdoe") {
		t.Fatalf("only asmith should remain pending: %+v", o)
	}
	e.comment("PROJ-1", "asmith", "good to go from my side")
	o = only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionPassed || !o.Changed || !strings.Contains(o.Detail, "3d") {
		t.Fatalf("expected pass reporting the wait: %+v", o)
	}
	if got := e.state("PROJ-1"); got != orchestrator.PRDApproved {
		t.Fatalf("state = %s", got)
	}
}

func TestSurvivesProcessRestartMidWait(t *testing.T) {
	e := newEnv(t)
	e.toReview("PROJ-1")
	e.rec.Tick(context.Background())
	e.clk.Advance(48 * time.Hour)

	// "restart": throw away every in-memory object and rebuild from the data directory only.
	st2 := e.open(e.dsn)
	rec2 := &Reconciler{Store: st2, Tracker: e.trk, Limits: lim, Approvers: approvers, Now: e.clk.Now}
	e.comment("PROJ-1", "jdoe", "approved")
	e.comment("PROJ-1", "asmith", "approved")
	if o := only(t, rec2.Tick(context.Background())); o.Action != ActionPassed {
		t.Fatalf("after restart: %+v", o)
	}
}

func TestApprovalPostedBeforeUploadIsIgnored(t *testing.T) {
	e := newEnv(t)
	e.comment("PROJ-1", "jdoe", "approved")
	e.comment("PROJ-1", "asmith", "approved")
	e.clk.Advance(time.Minute)
	e.toReview("PROJ-1") // PRD v1 attached AFTER those comments
	e.clk.Advance(time.Hour)
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionWaiting {
		t.Fatalf("early approvals must not count: %+v", o)
	}
}

// The dangerous race: a decision about v1 is computed, but while the tracker call is in flight
// a human sends it for revision and v2 is produced. Applying "v1 passed" to v2 would approve a
// document nobody reviewed.
func TestStaleDecisionCannotApproveANewerVersion(t *testing.T) {
	e := newEnv(t)
	e.toReview("PROJ-1")
	e.clk.Advance(time.Hour)
	e.comment("PROJ-1", "jdoe", "approved")
	e.comment("PROJ-1", "asmith", "approved")

	e.trk.hook = func() { // runs after the reconciler loaded v1, before it applies
		if _, err := e.st.Apply("PROJ-1", orchestrator.EvFeedback, "human asked for changes", lim); err != nil {
			t.Error(err)
		}
		e.clk.Advance(time.Minute)
		if _, err := e.st.AddArtifact("PROJ-1", "prd", []byte("# PRD v2")); err != nil {
			t.Error(err)
		}
		if _, err := e.st.Apply("PROJ-1", orchestrator.EvRevisionReady, "v2", lim); err != nil {
			t.Error(err)
		}
	}
	o := only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionStale || o.Changed {
		t.Fatalf("want stale, got %+v", o)
	}
	tk, _ := e.st.Load("PROJ-1")
	if tk.State != orchestrator.PRDReview || tk.Ctx.PRDVersion != 2 {
		t.Fatalf("v2 must remain in review: %s v%d", tk.State, tk.Ctx.PRDVersion)
	}
	// next pass: the old approvals predate v2's upload, so v2 is simply waiting again
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionWaiting {
		t.Fatalf("v2 should be waiting for fresh approvals: %+v", o)
	}
}

func TestTrackerOutageNeverChangesState(t *testing.T) {
	e := newEnv(t)
	e.toReview("PROJ-1")
	e.comment("PROJ-1", "jdoe", "approved")
	e.clk.Advance(time.Minute)
	e.comment("PROJ-1", "jdoe", "approved")
	e.comment("PROJ-1", "asmith", "approved")
	before, _ := e.st.Load("PROJ-1")

	e.trk.err = errors.New("jira 503")
	for i := 0; i < 5; i++ {
		o := only(t, e.rec.Tick(context.Background()))
		if o.Action != ActionError || o.Changed {
			t.Fatalf("outage must be an error outcome only: %+v", o)
		}
	}
	after, _ := e.st.Load("PROJ-1")
	if after.State != before.State || len(after.History) != len(before.History) {
		t.Fatal("outage changed the ticket")
	}

	e.trk.err = nil // tracker comes back days later: flow just continues
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionPassed {
		t.Fatalf("after recovery: %+v", o)
	}
}

func TestFeedbackSettlesThenSendsToRevision(t *testing.T) {
	e := newEnv(t)
	e.rec.FeedbackQuiet = 10 * time.Minute
	e.toReview("PROJ-1")
	e.clk.Advance(2 * time.Hour)
	e.comment("PROJ-1", "asmith", "/changes Please add an export requirement")

	e.clk.Advance(time.Minute)
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionSettling || o.Changed {
		t.Fatalf("fresh feedback should settle: %+v", o)
	}
	e.comment("PROJ-1", "asmith", "Also the success metric is vague") // reviewer still typing
	e.clk.Advance(9 * time.Minute)
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionSettling {
		t.Fatalf("second comment restarts the quiet period: %+v", o)
	}
	e.clk.Advance(2 * time.Minute)
	o := only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionFeedback || !o.Changed {
		t.Fatalf("after quiet period: %+v", o)
	}
	if got := e.state("PROJ-1"); got != orchestrator.PRDRevision {
		t.Fatalf("state = %s", got)
	}
	fb, err := e.st.LoadFeedback("PROJ-1", "prd", 1)
	if err != nil || len(fb.Changes) != 1 || len(fb.Discussion) != 1 {
		t.Fatalf("the request AND the follow-up remark must be saved for the revision step: %+v %v", fb, err)
	}
	if out := e.rec.Tick(context.Background()); len(out) != 0 {
		t.Fatalf("a ticket in revision is not the reconciler's business: %+v", out)
	}
}

func TestLaterApprovalsOutweighEarlierFeedback(t *testing.T) {
	e := newEnv(t)
	e.toReview("PROJ-1")
	e.clk.Advance(time.Hour)
	e.comment("PROJ-1", "asmith", "Can we clarify scope?")
	e.clk.Advance(time.Hour)
	e.comment("PROJ-1", "asmith", "approved")
	e.comment("PROJ-1", "jdoe", "lgtm")
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionPassed {
		t.Fatalf("approval after the question must pass: %+v", o)
	}
}

func TestTickIsIdempotent(t *testing.T) {
	e := newEnv(t)
	e.toReview("PROJ-1")
	e.clk.Advance(time.Hour)
	e.comment("PROJ-1", "jdoe", "approved")
	e.comment("PROJ-1", "asmith", "approved")
	only(t, e.rec.Tick(context.Background()))
	tk1, _ := e.st.Load("PROJ-1")
	for i := 0; i < 3; i++ {
		if out := e.rec.Tick(context.Background()); len(out) != 0 {
			t.Fatalf("repeat pass produced %+v", out)
		}
	}
	tk2, _ := e.st.Load("PROJ-1")
	if len(tk2.History) != len(tk1.History) {
		t.Fatal("repeat passes must not add history")
	}
}

// Several reconcilers (separate Store values, as separate processes would be) racing on the
// same data directory must produce exactly one transition.
func TestConcurrentReconcilersTransitionOnce(t *testing.T) {
	e := newEnv(t)
	e.toReview("PROJ-1")
	e.clk.Advance(time.Hour)
	e.comment("PROJ-1", "jdoe", "approved")
	e.comment("PROJ-1", "asmith", "approved")

	const n = 8
	var wg sync.WaitGroup
	results := make([][]Outcome, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := &Reconciler{Store: e.open(e.dsn), Tracker: e.trk, Limits: lim, Approvers: approvers, Now: e.clk.Now}
			results[i] = r.Tick(context.Background())
		}(i)
	}
	wg.Wait()
	changed := 0
	for _, rs := range results {
		for _, o := range rs {
			if o.Action == ActionError {
				t.Fatalf("race produced an error: %+v", o)
			}
			if o.Changed {
				changed++
			}
		}
	}
	tk, _ := e.st.Load("PROJ-1")
	passes := 0
	for _, h := range tk.History {
		if h.Event == orchestrator.EvGatePassed {
			passes++
		}
	}
	if changed != 1 || passes != 1 || tk.State != orchestrator.PRDApproved {
		t.Fatalf("changed=%d gate_passed history=%d state=%s", changed, passes, tk.State)
	}
}

func TestMisconfigurationIsSkippedNotFatal(t *testing.T) {
	e := newEnv(t)
	e.toReview("PROJ-1")
	e.rec.Approvers = func(string, string) ([]string, gates.Quorum) { return nil, gates.QuorumAll }
	o := only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionSkipped || !strings.Contains(o.Detail, "no approvers") {
		t.Fatalf("%+v", o)
	}
	if got := e.state("PROJ-1"); got != orchestrator.PRDReview {
		t.Fatalf("state = %s", got)
	}

	// state says v2 but only v1 exists
	e2 := newEnv(t)
	e2.toReview("PROJ-2")
	e2.st.Apply("PROJ-2", orchestrator.EvFeedback, "", lim)
	e2.st.Apply("PROJ-2", orchestrator.EvRevisionReady, "", lim)
	o = only(t, e2.rec.Tick(context.Background()))
	if o.Action != ActionSkipped || !strings.Contains(o.Detail, "v1") {
		t.Fatalf("%+v", o)
	}
}

func TestOneBrokenTicketDoesNotBlockOthers(t *testing.T) {
	e := newEnv(t)
	e.toReview("PROJ-1")
	e.toReview("PROJ-2")
	e.clk.Advance(time.Hour)
	e.comment("PROJ-2", "jdoe", "approved")
	e.comment("PROJ-2", "asmith", "approved")
	e.rec.Approvers = func(key, gate string) ([]string, gates.Quorum) {
		if key == "PROJ-1" {
			return nil, gates.QuorumAll // PROJ-1 is misconfigured
		}
		return approvers(key, gate)
	}
	got := map[string]Action{}
	for _, o := range e.rec.Tick(context.Background()) {
		got[o.Key] = o.Action
	}
	if got["PROJ-1"] != ActionSkipped || got["PROJ-2"] != ActionPassed {
		t.Fatalf("%v", got)
	}
}

func TestAgeFormatting(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second: "30s", 35 * time.Minute: "35m", 5*time.Hour + 10*time.Minute: "5h 10m",
		52 * time.Hour: "2d 4h", -time.Hour: "0s",
	}
	for d, want := range cases {
		if got := Age(d); got != want {
			t.Errorf("Age(%v) = %q, want %q", d, got, want)
		}
	}
}
