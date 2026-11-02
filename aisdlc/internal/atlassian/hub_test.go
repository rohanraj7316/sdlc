package atlassian

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"aisdlc/internal/bitbucket"
	"aisdlc/internal/bitbucket/bbtest"
	"aisdlc/internal/config"
	"aisdlc/internal/gates"
	"aisdlc/internal/httpx"
	"aisdlc/internal/jira"
	"aisdlc/internal/jira/jiratest"
	"aisdlc/internal/orchestrator"
	"aisdlc/internal/reconcile"
	"aisdlc/internal/store"
	"aisdlc/internal/store/storetest"
	"aisdlc/internal/tracker"
)

var lim = orchestrator.Limits{ReviewRounds: 2, BuildAttempts: 2}

type world struct {
	t   *testing.T
	jr  *jiratest.Server
	bb  *bbtest.Server
	st  *store.Store
	hub *Hub
	cfg config.Config
	clk *clock
}

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

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, clk: &clock{t: time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)}}
	w.jr = jiratest.New("jt", "aisdlc-bot")
	w.bb = bbtest.New("bt", "aisdlc-bot")
	w.bb.AddRepo("PAY", "payments-api")
	t.Cleanup(w.jr.Close)
	t.Cleanup(w.bb.Close)
	w.jr.Now, w.bb.Now = w.clk.Now, w.clk.Now
	var err error
	if w.st, err = store.OpenWithClock(storetest.DSN(t), w.clk.Now); err != nil {
		t.Fatal(err)
	}
	nosleep := func(h *httpx.Client) { h.Sleep = func(time.Duration) {} }
	w.hub = &Hub{Jira: jira.New(w.jr.URL, "jt").WithHTTP(nosleep), BB: bitbucket.New(w.bb.URL, "bt").WithHTTP(nosleep), Store: w.st}
	w.cfg = config.Default()
	w.cfg.Approvers = map[string]config.GateApprovers{"PROJ": {
		PRD: config.ApproverSet{Required: []string{"jdoe", "asmith"}},
		PR:  config.ApproverSet{Required: []string{"rev1"}, Quorum: "any"},
		QA:  config.ApproverSet{Required: []string{"qa1"}},
	}}
	w.cfg.Participants = map[string]string{"jdoe": "product", "asmith": "product", "dev1": "developer", "qa1": "qa", "rev1": "code reviewer"}
	return w
}

func (w *world) rec(ans reconcile.Answerer) *reconcile.Reconciler {
	return &reconcile.Reconciler{Store: w.st, Tracker: w.hub, Limits: lim, Approvers: w.cfg.ApproversFor,
		Now: w.clk.Now, Roles: w.cfg.RoleOf, Bot: "aisdlc-bot", Answerer: ans}
}

func (w *world) walk(key string, evs ...orchestrator.Event) {
	w.t.Helper()
	for _, ev := range evs {
		if _, err := w.st.Apply(key, ev, string(ev), lim); err != nil {
			w.t.Fatalf("%s: %v", ev, err)
		}
	}
}

func (w *world) sub(key string, th tracker.Thread) string {
	w.t.Helper()
	tk, err := w.st.Load(key)
	if err != nil || tk.Subtasks[string(th)] == "" {
		w.t.Fatalf("no %s sub-task recorded for %s: %v", th, key, err)
	}
	return tk.Subtasks[string(th)]
}

func (w *world) say(subKey, author, body string) {
	w.clk.Advance(time.Second)
	w.jr.Say(subKey, author, body)
}

type answerer struct {
	mu sync.Mutex
	n  int
}

func (a *answerer) Answer(_ context.Context, r reconcile.AnswerRequest) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	return "ANSWER to " + r.Question.ID, nil
}

func TestIntakeRegistersTicketsAndCreatesThreadsOnce(t *testing.T) {
	w := newWorld(t)
	w.jr.AddIssue("PROJ-1", "Auto reconcile", "Analysts reconcile by hand.", "ai-sdlc")
	w.jr.AddIssue("PROJ-2", "Other", "no label")
	ctx := context.Background()

	keys, err := w.hub.Intake(ctx, "ai-sdlc", "")
	if err != nil || len(keys) != 1 || keys[0] != "PROJ-1" {
		t.Fatalf("%v %v", keys, err)
	}
	if tk, _ := w.st.Load("PROJ-1"); tk.State != orchestrator.New || len(tk.Subtasks) != 3 {
		t.Fatalf("%+v", tk)
	}
	subs := w.jr.SubtasksOf("PROJ-1")
	if len(subs) != 3 {
		t.Fatalf("want 3 sub-tasks, got %v", subs)
	}
	var prd string
	for _, k := range subs {
		if strings.HasPrefix(w.jr.Get(k).Summary, "[PRD] ") {
			prd = k
		}
	}
	if is := w.jr.Get(prd); len(is.Comments) != 1 || !strings.Contains(is.Comments[0].Body, "/changes") || !strings.Contains(is.Comments[0].Body, "/ask") {
		t.Fatalf("the PRD thread must open with the rules of the conversation: %+v", is.Comments)
	}
	// running it again, even from a fresh process state, creates nothing new
	if keys, err := w.hub.Intake(ctx, "ai-sdlc", ""); err != nil || len(keys) != 0 {
		t.Fatalf("%v %v", keys, err)
	}
	if len(w.jr.SubtasksOf("PROJ-1")) != 3 || len(w.jr.Get(prd).Comments) != 1 {
		t.Fatal("intake must be idempotent")
	}
}

func TestEnsureThreadsAdoptsExistingSubtasks(t *testing.T) {
	w := newWorld(t)
	w.jr.AddIssue("PROJ-1", "x", "y", "ai-sdlc")
	pre, _ := w.hub.Jira.CreateSubtask(context.Background(), "PROJ-1", "[TECH] made by a human", "Sub-task")
	made, err := w.hub.EnsureThreads(context.Background(), "PROJ-1")
	if err != nil || len(made) != 2 {
		t.Fatalf("only PRD and QA should be created: %v %v", made, err)
	}
	if got := w.sub("PROJ-1", tracker.ThreadTech); got != pre {
		t.Fatalf("the existing [TECH] sub-task must be adopted, got %s", got)
	}
}

func TestPublishAttachesTheDocumentAndExplainsIt(t *testing.T) {
	w := newWorld(t)
	w.jr.AddIssue("PROJ-1", "x", "y", "ai-sdlc")
	ctx := context.Background()
	if err := w.hub.Publish(ctx, "PROJ-1", tracker.ThreadPRD, "PROJ-1_PRD_v1.md", []byte("# PRD"), "PRD v1 is ready"); err != nil {
		t.Fatal(err)
	}
	is := w.jr.Get(w.sub("PROJ-1", tracker.ThreadPRD))
	if len(is.Attachments) != 1 || is.Attachments[0] != "PROJ-1_PRD_v1.md" || len(is.Comments) != 1 || is.Comments[0].Body != "PRD v1 is ready" {
		t.Fatalf("%+v", is)
	}
}

func TestSeedFromTheTicket(t *testing.T) {
	w := newWorld(t)
	w.jr.AddIssue("PROJ-1", "Auto reconcile", "Analysts reconcile by hand.")
	title, text, err := w.hub.Seed(context.Background(), "PROJ-1")
	if err != nil || title != "Auto reconcile" || !strings.Contains(text, "Analysts reconcile by hand.") {
		t.Fatalf("%q %q %v", title, text, err)
	}
}

func TestThreadsStayIndependentAndPostingGoesToTheRightPlace(t *testing.T) {
	w := newWorld(t)
	w.jr.AddIssue("PROJ-1", "x", "y", "ai-sdlc")
	ctx := context.Background()
	w.hub.EnsureThreads(ctx, "PROJ-1")
	w.say(w.sub("PROJ-1", tracker.ThreadPRD), "jdoe", "about the PRD")
	w.say(w.sub("PROJ-1", tracker.ThreadQA), "qa1", "about QA")
	if err := w.hub.Post(ctx, "PROJ-1", tracker.ThreadTech, gates.Comment{Body: "tech note", ReplyTo: "77"}); err != nil {
		t.Fatal(err)
	}
	for th, want := range map[tracker.Thread]string{tracker.ThreadPRD: "about the PRD", tracker.ThreadQA: "about QA", tracker.ThreadTech: "tech note"} {
		cs, err := w.hub.Comments(ctx, "PROJ-1", th)
		if err != nil || len(cs) != 1 || !strings.HasPrefix(cs[0].Body, want) {
			t.Fatalf("%s: %+v %v", th, cs, err)
		}
	}
	if cs, _ := w.hub.Comments(ctx, "PROJ-1", tracker.ThreadTech); cs[0].ReplyTo != "77" {
		t.Fatalf("reply marker must round-trip: %+v", cs[0])
	}
	// no pull request yet: empty thread, open state, and nowhere to post
	if cs, err := w.hub.Comments(ctx, "PROJ-1", tracker.ThreadPR); err != nil || len(cs) != 0 {
		t.Fatalf("%v %v", cs, err)
	}
	if st, err := w.hub.PRState(ctx, "PROJ-1"); err != nil || st != tracker.PROpen {
		t.Fatalf("%v %v", st, err)
	}
	if err := w.hub.Post(ctx, "PROJ-1", tracker.ThreadPR, gates.Comment{Body: "x"}); err == nil {
		t.Fatal("posting to a PR that does not exist must fail loudly")
	}
}

// The real reconciler, driven through simulated Jira and Bitbucket from first label to QA.
func TestWholeConversationAcrossJiraAndBitbucket(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	const key = "PROJ-1"
	w.jr.AddIssue(key, "Auto reconcile", "Analysts reconcile reports by hand.", "ai-sdlc")
	if _, err := w.hub.Intake(ctx, "ai-sdlc", ""); err != nil {
		t.Fatal(err)
	}

	// PRD v1 is produced and published to the [PRD] sub-task
	w.walk(key, orchestrator.EvStart)
	art, _ := w.st.AddArtifact(key, "prd", []byte("# PRD v1"))
	w.walk(key, orchestrator.EvInterviewDone)
	if err := w.hub.Publish(ctx, key, tracker.ThreadPRD, "PROJ-1_PRD_v1.md", []byte("# PRD v1"), "PRD v1 is ready"); err != nil {
		t.Fatal(err)
	}
	w.st.MarkPublished(key, "prd", art.Version)
	prd := w.sub(key, tracker.ThreadPRD)

	// people talk; a question is answered once, in Jira; nothing moves
	ans := &answerer{}
	r := w.rec(ans)
	w.say(prd, "dev1", "the export looks easy")
	w.say(prd, "qa1", "/ask what is the export format?")
	w.say(prd, "jdoe", "can we clarify scope?")
	for i := 0; i < 3; i++ {
		r.Tick(ctx)
	}
	if ans.n != 1 {
		t.Fatalf("answered %d times", ans.n)
	}
	var bot int
	for _, c := range w.jr.Get(prd).Comments {
		if c.Author == "aisdlc-bot" && strings.Contains(c.Body, "ANSWER to") && strings.Contains(c.Body, "in reply to comment") {
			bot++
		}
	}
	if bot != 1 {
		t.Fatalf("exactly one reply must be in the Jira thread: %+v", w.jr.Get(prd).Comments)
	}
	if tk, _ := w.st.Load(key); tk.State != orchestrator.PRDReview {
		t.Fatalf("conversation alone moved the ticket to %s", tk.State)
	}

	// both approvers sign off (their comments are in Jira) -> PRD approved
	w.say(prd, "jdoe", "okay, done")
	w.say(prd, "asmith", "good to go from my side")
	r.Tick(ctx)
	if tk, _ := w.st.Load(key); tk.State != orchestrator.PRDApproved {
		t.Fatalf("state = %s", tk.State)
	}

	// jump to an open pull request (the stage runners that get here come next)
	w.walk(key, orchestrator.EvProceed)
	w.st.AddArtifact(key, "tech", []byte("# tech"))
	w.walk(key, orchestrator.EvTechConfirmed, orchestrator.EvGatePassed, orchestrator.EvProceed,
		orchestrator.EvBuildDone, orchestrator.EvVerifyPass, orchestrator.EvReviewClean, orchestrator.EvPushed)
	repo := bitbucket.Repo{Project: "PAY", Slug: "payments-api"}
	pr, err := w.hub.BB.CreatePR(ctx, repo, key, "main", key+": reconcile", "desc", []string{"rev1"})
	if err != nil {
		t.Fatal(err)
	}
	w.st.SetPR(key, store.PRRef{Project: "PAY", Repo: "payments-api", ID: pr.ID, Branch: key})

	// a code reviewer asks for changes in Bitbucket -> PR_REWORK, conversation saved
	w.clk.Advance(time.Second)
	w.bb.Say("PAY/payments-api", pr.ID, "rev1", "/changes rename Foo to Bar", 0)
	r.Tick(ctx)
	tk, _ := w.st.Load(key)
	if tk.State != orchestrator.PRRework || tk.Ctx.PRRound != 1 {
		t.Fatalf("%s %+v", tk.State, tk.Ctx)
	}
	if fb, err := w.st.LoadFeedback(key, "pr", 1); err != nil || len(fb.Changes) != 1 || fb.Changes[0].Author != "rev1" {
		t.Fatalf("%+v %v", fb, err)
	}

	// the builder fixes and pushes (simulated); then the PR is merged in Bitbucket
	w.walk(key, orchestrator.EvFixed, orchestrator.EvVerifyPass, orchestrator.EvReviewClean, orchestrator.EvPushed)
	w.bb.SetState("PAY/payments-api", pr.ID, "MERGED")
	r.Tick(ctx)
	if tk, _ = w.st.Load(key); tk.State != orchestrator.Merged {
		t.Fatalf("merge must be read from Bitbucket: %s", tk.State)
	}

	// QA is started from the [QA] sub-task in Jira
	qa := w.sub(key, tracker.ThreadQA)
	w.say(qa, "dev1", "/qa")
	r.Tick(ctx)
	if tk, _ = w.st.Load(key); tk.State != orchestrator.Merged {
		t.Fatalf("a developer is not a QA owner: %s", tk.State)
	}
	w.say(qa, "qa1", "/qa")
	r.Tick(ctx)
	if tk, _ = w.st.Load(key); tk.State != orchestrator.QARunning {
		t.Fatalf("state = %s", tk.State)
	}
	_ = fmt.Sprint
}

func TestDocNote(t *testing.T) {
	n := DocNote("prd", 2, 1, "- added export")
	for _, want := range []string{"PRD v2", "replaces v1", "earlier approvals do not carry over", "- added export", `"approved"`, "/changes", "/ask"} {
		if !strings.Contains(n, want) {
			t.Errorf("note missing %q:\n%s", want, n)
		}
	}
	if n := DocNote("tech", 1, 0, ""); strings.Contains(n, "replaces") || strings.Contains(n, "Changelog") || !strings.Contains(n, "Technical plan v1") {
		t.Errorf("first version has nothing to replace:\n%s", n)
	}
}
