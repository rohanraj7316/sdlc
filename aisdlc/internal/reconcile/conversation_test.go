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
	"aisdlc/internal/tracker"
)

type fakeAnswerer struct {
	mu    sync.Mutex
	calls []AnswerRequest
	fail  int // fail this many calls first
}

func (a *fakeAnswerer) Answer(_ context.Context, req AnswerRequest) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, req)
	if a.fail > 0 {
		a.fail--
		return "", errors.New("agent busy")
	}
	return "ANSWER to " + req.Question.ID, nil
}

func (a *fakeAnswerer) count() int { a.mu.Lock(); defer a.mu.Unlock(); return len(a.calls) }

var roleMap = map[string]string{"jdoe": "product", "asmith": "product", "dev1": "developer", "qa1": "qa", "rev1": "code reviewer", "rev2": "code reviewer"}

func (e *env) withConversation() *fakeAnswerer {
	ans := &fakeAnswerer{}
	e.rec.Answerer, e.rec.Bot = ans, "aisdlc-bot"
	e.rec.Roles = func(u string) string {
		if r, ok := roleMap[u]; ok {
			return r
		}
		return "participant"
	}
	return ans
}

// say posts a comment one second after the previous event, so ordering is unambiguous.
func (e *env) say(key, thread, author, body string) gates.Comment {
	e.clk.Advance(time.Second)
	c := gates.Comment{ID: author + e.clk.Now().Format("150405.000000"), Author: author, Created: e.clk.Now(), Body: body}
	e.trk.add(key, thread, c)
	return c
}

func (e *env) walk(key string, evs ...orchestrator.Event) {
	e.t.Helper()
	for _, ev := range evs {
		if _, err := e.st.Apply(key, ev, string(ev), lim); err != nil {
			e.t.Fatalf("%s: %v", ev, err)
		}
	}
}

// toPROpen walks a ticket through to an open pull request, attaching a "pr" artifact at push time.
func (e *env) toPROpen(key string) {
	e.t.Helper()
	e.toReview(key)
	e.walk(key, orchestrator.EvGatePassed, orchestrator.EvProceed)
	e.st.AddArtifact(key, "tech", []byte("# tech"))
	e.walk(key, orchestrator.EvTechConfirmed, orchestrator.EvGatePassed, orchestrator.EvProceed,
		orchestrator.EvBuildDone, orchestrator.EvVerifyPass, orchestrator.EvReviewClean)
	e.push(key)
}

func (e *env) push(key string) {
	e.t.Helper()
	e.clk.Advance(time.Minute)
	if _, err := e.st.AddArtifact(key, "pr", []byte("pr description")); err != nil {
		e.t.Fatal(err)
	}
	e.walk(key, orchestrator.EvPushed)
}

func (e *env) comments(key, thread string) []gates.Comment {
	cs, _ := e.trk.Comments(context.Background(), key, tracker.Thread(thread))
	return cs
}

// ---- point: a question must not become a rewrite -----------------------------------------

func TestQuestionsAndRemarksAreDiscussionNeverARewrite(t *testing.T) {
	e := newEnv(t)
	e.toReview("PROJ-1")
	e.say("PROJ-1", "prd", "jdoe", "Can we clarify what is out of scope?") // an approver asks a question
	e.say("PROJ-1", "prd", "dev1", "the export looks easy to me")          // a developer weighs in
	e.say("PROJ-1", "prd", "asmith", "done, but metrics worry me")         // not a clean approval, not a command
	for i := 0; i < 3; i++ {
		o := only(t, e.rec.Tick(context.Background()))
		if o.Action != ActionWaiting || o.Changed || !strings.Contains(o.Detail, "3 comment(s) of discussion") {
			t.Fatalf("pass %d: %+v", i, o)
		}
	}
	if got := e.state("PROJ-1"); got != orchestrator.PRDReview {
		t.Fatalf("state = %s: a conversation alone must not move the ticket", got)
	}
}

func TestOnlyDesignatedApproversCanRequestChanges(t *testing.T) {
	e := newEnv(t)
	e.toReview("PROJ-1")
	e.say("PROJ-1", "prd", "dev1", "/changes rewrite the whole thing")
	e.say("PROJ-1", "prd", "stranger", "/changes me too")
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionWaiting {
		t.Fatalf("non-approvers' commands are just discussion: %+v", o)
	}
	e.say("PROJ-1", "prd", "asmith", "/changes add an export requirement")
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionFeedback || !o.Changed {
		t.Fatalf("an approver's /changes must work: %+v", o)
	}
}

// ---- point: the revision must see the wider discussion -----------------------------------

func TestChangeRequestCapturesTheWholeConversationWithRoles(t *testing.T) {
	e := newEnv(t)
	e.withConversation()
	e.toReview("PROJ-1")
	e.say("PROJ-1", "prd", "dev1", "technically the export is easy, CSV only")
	e.say("PROJ-1", "prd", "qa1", "/ask what is the expected export format?")
	e.say("PROJ-1", "prd", "asmith", "/changes add an export requirement")
	o := only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionFeedback || !strings.Contains(o.Detail, "1 change request(s) from asmith and 2 other comment(s)") {
		t.Fatalf("%+v", o)
	}
	fb, err := e.st.LoadFeedback("PROJ-1", "prd", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(fb.Changes) != 1 || fb.Changes[0].Author != "asmith" || len(fb.Discussion) != 2 {
		t.Fatalf("%+v", fb)
	}
	if fb.Roles["dev1"] != "developer" || fb.Roles["qa1"] != "qa" || fb.Roles["asmith"] != "product" {
		t.Fatalf("roles must be captured so the writer knows who said what: %v", fb.Roles)
	}
}

// ---- point: the AI must be able to reply in the thread -----------------------------------

func TestAskIsAnsweredInTheThreadExactlyOnce(t *testing.T) {
	e := newEnv(t)
	ans := e.withConversation()
	e.toReview("PROJ-1")
	q := e.say("PROJ-1", "prd", "qa1", "/ask what is out of scope?")

	o := only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionAnswered || !strings.Contains(o.Detail, "answered 1 question(s)") || o.Changed {
		t.Fatalf("%+v", o)
	}
	cs := e.comments("PROJ-1", "prd")
	last := cs[len(cs)-1]
	if last.Author != "aisdlc-bot" || last.ReplyTo != q.ID || !strings.HasPrefix(last.Body, "AI assistant reply to qa1:") || !strings.Contains(last.Body, "ANSWER to "+q.ID) {
		t.Fatalf("reply = %+v", last)
	}
	if call := ans.calls[0]; call.Gate != "prd" || call.Document != "# PRD v1" || call.Question.ID != q.ID {
		t.Fatalf("the answerer must get the document under review and the question: %+v", call)
	}
	for i := 0; i < 3; i++ { // later passes must not answer again, even though the question is still there
		if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionWaiting {
			t.Fatalf("pass %d: %+v", i, o)
		}
	}
	if ans.count() != 1 || e.trk.posts != 1 {
		t.Fatalf("answered %d times, posted %d times", ans.count(), e.trk.posts)
	}
	e.say("PROJ-1", "prd", "dev1", "/ask and what about pricing?")
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionAnswered || ans.count() != 2 {
		t.Fatalf("a new question gets its own answer: %+v (%d calls)", o, ans.count())
	}
}

func TestFailedAnswerIsRetriedNotLost(t *testing.T) {
	e := newEnv(t)
	ans := e.withConversation()
	ans.fail = 1
	e.toReview("PROJ-1")
	e.say("PROJ-1", "prd", "qa1", "/ask what is out of scope?")
	o := only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionWaiting || !strings.Contains(o.Detail, "answering failed, will retry") || e.trk.posts != 0 {
		t.Fatalf("%+v posts=%d", o, e.trk.posts)
	}
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionAnswered || e.trk.posts != 1 {
		t.Fatalf("the retry must answer: %+v posts=%d", o, e.trk.posts)
	}
}

func TestPostFailureAlsoRetries(t *testing.T) {
	e := newEnv(t)
	ans := e.withConversation()
	e.toReview("PROJ-1")
	e.say("PROJ-1", "prd", "qa1", "/ask what is out of scope?")
	e.trk.postErr = errors.New("jira 503")
	if o := only(t, e.rec.Tick(context.Background())); !strings.Contains(o.Detail, "answering failed") {
		t.Fatalf("%+v", o)
	}
	e.trk.postErr = nil
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionAnswered || e.trk.posts != 1 || ans.count() != 2 {
		t.Fatalf("%+v posts=%d calls=%d", o, e.trk.posts, ans.count())
	}
}

func TestReadOnlyTrackerMeansNoAnswersButEverythingElseWorks(t *testing.T) {
	e := newEnv(t)
	ans := e.withConversation()
	e.rec.Tracker = tracker.Func(e.trk.Comments) // can read, cannot Post
	e.toReview("PROJ-1")
	e.say("PROJ-1", "prd", "qa1", "/ask what is out of scope?")
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionWaiting || ans.count() != 0 {
		t.Fatalf("%+v calls=%d", o, ans.count())
	}
	e.say("PROJ-1", "prd", "jdoe", "approved")
	e.say("PROJ-1", "prd", "asmith", "approved")
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionPassed {
		t.Fatalf("gating must not depend on the AI: %+v", o)
	}
}

func TestSeveralReconcilersAnswerAQuestionOnce(t *testing.T) {
	e := newEnv(t)
	ans := e.withConversation()
	e.toReview("PROJ-1")
	e.say("PROJ-1", "prd", "qa1", "/ask what is out of scope?")
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &Reconciler{Store: e.open(e.dsn), Tracker: e.trk, Limits: lim, Approvers: approvers, Now: e.clk.Now, Answerer: ans, Bot: "aisdlc-bot"}
			r.Tick(context.Background())
		}()
	}
	wg.Wait()
	if ans.count() != 1 || e.trk.posts != 1 {
		t.Fatalf("answered %d times, posted %d times; want once", ans.count(), e.trk.posts)
	}
}

func TestQuestionsAreAnsweredWhileChangesSettle(t *testing.T) {
	e := newEnv(t)
	ans := e.withConversation()
	e.rec.FeedbackQuiet = 10 * time.Minute
	e.toReview("PROJ-1")
	e.say("PROJ-1", "prd", "asmith", "/changes add export")
	e.say("PROJ-1", "prd", "dev1", "/ask is CSV enough?")
	o := only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionSettling || ans.count() != 1 || !strings.Contains(o.Detail, "answered 1 question(s)") {
		t.Fatalf("%+v calls=%d", o, ans.count())
	}
	// the bot's own answer counts as conversation, so it also restarts the quiet period
	e.clk.Advance(11 * time.Minute)
	o = only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionFeedback {
		t.Fatalf("%+v", o)
	}
	fb, _ := e.st.LoadFeedback("PROJ-1", "prd", 1)
	var sawAnswer bool
	for _, c := range fb.Discussion {
		sawAnswer = sawAnswer || c.Author == "aisdlc-bot"
	}
	if !sawAnswer || fb.Roles["aisdlc-bot"] != "AI assistant" {
		t.Fatalf("the revision must see the AI's own answer too: %+v", fb)
	}
}

func TestTechGateUsesItsOwnApproversAndThread(t *testing.T) {
	e := newEnv(t)
	ans := e.withConversation()
	e.toReview("PROJ-1")
	e.walk("PROJ-1", orchestrator.EvGatePassed, orchestrator.EvProceed)
	e.st.AddArtifact("PROJ-1", "tech", []byte("# TECH v1"))
	e.walk("PROJ-1", orchestrator.EvTechConfirmed)
	e.say("PROJ-1", "prd", "jdoe", "approved") // wrong thread: must not count for the tech gate
	e.say("PROJ-1", "tech", "qa1", "/ask which repos change?")
	o := only(t, e.rec.Tick(context.Background()))
	if o.Gate != "tech" || o.Action != ActionAnswered || ans.calls[0].Document != "# TECH v1" {
		t.Fatalf("%+v", o)
	}
	e.say("PROJ-1", "tech", "dev1", "approved")
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionPassed || e.state("PROJ-1") != orchestrator.TechApproved {
		t.Fatalf("%+v state=%s", o, e.state("PROJ-1"))
	}
}

// ---- point: human code review needs a loop back to the builder ----------------------------

func TestHumanCodeReviewRoundsLoopBackToTheBuilder(t *testing.T) {
	e := newEnv(t)
	e.withConversation()
	e.toPROpen("PROJ-1")

	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionWaiting || o.Gate != "pr" || !strings.Contains(o.Detail, "round 1") {
		t.Fatalf("%+v", o)
	}
	e.say("PROJ-1", "pr", "dev1", "/changes this is wrong") // not a code reviewer: just discussion
	e.say("PROJ-1", "pr", "rev1", "why a new helper here?") // a question, not a request
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionWaiting {
		t.Fatalf("%+v", o)
	}
	e.say("PROJ-1", "pr", "rev1", "/changes rename Foo to Bar and add a test for the empty case")
	o := only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionFeedback || !o.Changed {
		t.Fatalf("%+v", o)
	}
	tk, _ := e.st.Load("PROJ-1")
	if tk.State != orchestrator.PRRework || tk.Ctx.PRRound != 1 {
		t.Fatalf("%s %+v", tk.State, tk.Ctx)
	}
	fb, err := e.st.LoadFeedback("PROJ-1", "pr", 1)
	if err != nil || len(fb.Changes) != 1 || len(fb.Discussion) != 2 || fb.Roles["rev1"] != "code reviewer" {
		t.Fatalf("round 1 feedback: %+v %v", fb, err)
	}

	// the builder fixes it, AI re-review is clean, the new commits are pushed to the SAME pull request
	e.walk("PROJ-1", orchestrator.EvFixed, orchestrator.EvVerifyPass, orchestrator.EvReviewClean)
	e.push("PROJ-1")
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionWaiting || !strings.Contains(o.Detail, "round 2") {
		t.Fatalf("round-1 comments are history after a push: %+v", o)
	}
	e.say("PROJ-1", "pr", "rev2", "/changes also update the README")
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionFeedback {
		t.Fatalf("%+v", o)
	}
	fb2, err := e.st.LoadFeedback("PROJ-1", "pr", 2)
	if err != nil || len(fb2.Changes) != 1 || fb2.Changes[0].Author != "rev2" || len(fb2.Discussion) != 0 {
		t.Fatalf("round 2 must contain only round-2 conversation: %+v %v", fb2, err)
	}
	if tk, _ = e.st.Load("PROJ-1"); tk.Ctx.PRRound != 2 {
		t.Fatalf("rounds are counted, never capped: %+v", tk.Ctx)
	}
}

func TestMergeAndDeclineAreReadFromTheCodeHost(t *testing.T) {
	e := newEnv(t)
	e.toPROpen("PROJ-1")
	e.toPROpen("PROJ-2")
	e.trk.pr = tracker.PRMerged
	got := map[string]Outcome{}
	for _, o := range e.rec.Tick(context.Background()) {
		got[o.Key] = o
	}
	if got["PROJ-1"].Action != ActionAdvanced || e.state("PROJ-1") != orchestrator.Merged || e.state("PROJ-2") != orchestrator.Merged {
		t.Fatalf("%+v states %s %s", got, e.state("PROJ-1"), e.state("PROJ-2"))
	}
	e.toPROpen("PROJ-3")
	e.trk.pr = tracker.PRDeclined
	e.rec.Tick(context.Background())
	if tk, _ := e.st.Load("PROJ-3"); tk.State != orchestrator.Escalated || !strings.Contains(tk.Ctx.EscalationReason, "declined") {
		t.Fatalf("%s %+v", tk.State, tk.Ctx)
	}
}

// ---- point: QA is a conversation too --------------------------------------------------------

func (e *env) toMerged(key string) {
	e.t.Helper()
	e.toPROpen(key)
	e.walk(key, orchestrator.EvMerged)
}

func TestQAStartsOnlyWhenAnOwnerSaysSoAfterTheMerge(t *testing.T) {
	e := newEnv(t)
	e.say("PROJ-1", "qa", "qa1", "/qa (written before the merge: history)")
	e.toMerged("PROJ-1")
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionWaiting || o.Gate != "qa" {
		t.Fatalf("%+v", o)
	}
	e.say("PROJ-1", "qa", "dev1", "/qa please")
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionWaiting {
		t.Fatalf("a developer is not a QA owner: %+v", o)
	}
	e.say("PROJ-1", "qa", "qa1", "/qa")
	o := only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionAdvanced || !o.Changed {
		t.Fatalf("%+v", o)
	}
	if tk, _ := e.st.Load("PROJ-1"); tk.State != orchestrator.QARunning || tk.Ctx.QARun != 1 {
		t.Fatalf("%s %+v", tk.State, tk.Ctx)
	}
}

func (e *env) toQAFailed(key string) {
	e.t.Helper()
	e.toMerged(key)
	e.walk(key, orchestrator.EvQATrigger)
	e.clk.Advance(time.Minute)
	e.st.AddArtifact(key, "qa", []byte("QA report: 2 failures"))
	e.walk(key, orchestrator.EvQAFail)
}

func TestQAFailureTalkThenFixGoesBackToTheDeveloper(t *testing.T) {
	e := newEnv(t)
	e.toQAFailed("PROJ-1")
	e.say("PROJ-1", "qa", "qa1", "the export drops the last row")
	e.say("PROJ-1", "qa", "dev1", "reproduced, off-by-one in the writer")
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionWaiting || !strings.Contains(o.Detail, "2 comment(s) of discussion") {
		t.Fatalf("talking alone moves nothing: %+v", o)
	}
	e.say("PROJ-1", "qa", "qa1", "/fix agreed, please fix the off-by-one")
	o := only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionFeedback || !o.Changed {
		t.Fatalf("%+v", o)
	}
	tk, _ := e.st.Load("PROJ-1")
	if tk.State != orchestrator.Building || tk.Ctx.QARun != 1 {
		t.Fatalf("%s %+v", tk.State, tk.Ctx)
	}
	fb, err := e.st.LoadFeedback("PROJ-1", "qa", 1)
	if err != nil || len(fb.Changes) != 1 || len(fb.Discussion) != 2 {
		t.Fatalf("the developer must get the findings AND the discussion: %+v %v", fb, err)
	}
}

func TestQAFailureRetestWithoutAFixAndTheLatestCommandWins(t *testing.T) {
	e := newEnv(t)
	e.toQAFailed("PROJ-1")
	e.say("PROJ-1", "qa", "qa1", "/qa flaky environment, run it again")
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionAdvanced || e.state("PROJ-1") != orchestrator.QARunning {
		t.Fatalf("%+v %s", o, e.state("PROJ-1"))
	}
	if tk, _ := e.st.Load("PROJ-1"); tk.Ctx.QARun != 2 {
		t.Fatalf("a retest is a new run: %+v", tk.Ctx)
	}

	e2 := newEnv(t)
	e2.toQAFailed("PROJ-2")
	e2.say("PROJ-2", "qa", "qa1", "/fix real bug")
	e2.say("PROJ-2", "qa", "qa1", "/qa actually, let me retest first")
	e2.rec.Tick(context.Background())
	if got := e2.state("PROJ-2"); got != orchestrator.QARunning {
		t.Fatalf("the newer /qa must win over the older /fix, got %s", got)
	}
}

func TestQASignOffClosesTheTicketReadyForYourDeployment(t *testing.T) {
	e := newEnv(t)
	e.toMerged("PROJ-1")
	e.walk("PROJ-1", orchestrator.EvQATrigger)
	e.clk.Advance(time.Minute)
	e.st.AddArtifact("PROJ-1", "qa", []byte("QA report: all green"))
	e.walk("PROJ-1", orchestrator.EvQAPass)
	e.say("PROJ-1", "qa", "dev1", "approved") // not a QA owner
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionWaiting || !strings.Contains(o.Detail, "qa1") {
		t.Fatalf("%+v", o)
	}
	e.say("PROJ-1", "qa", "qa1", "okay, done")
	o := only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionAdvanced || !strings.Contains(o.Detail, "ready for you to deploy") || e.state("PROJ-1") != orchestrator.Done {
		t.Fatalf("%+v %s", o, e.state("PROJ-1"))
	}
}

// A QA owner who is not configured must produce a clear message, not silence.
func TestMissingQAOwnersAreReportedNotIgnored(t *testing.T) {
	e := newEnv(t)
	e.rec.Approvers = func(string, string) ([]string, gates.Quorum) { return nil, gates.QuorumAll }
	e.toMerged("PROJ-1")
	if o := only(t, e.rec.Tick(context.Background())); o.Action != ActionSkipped || !strings.Contains(o.Detail, "no QA owners") {
		t.Fatalf("%+v", o)
	}
	var _ = store.ErrStale
}

// The assistant's status notes ("PRD v1 is attached...") are noise to the writer; its answers
// to questions are not.
func TestBotHousekeepingNotesAreNotDiscussionButItsAnswersAre(t *testing.T) {
	e := newEnv(t)
	e.withConversation()
	e.toReview("PROJ-1")
	e.say("PROJ-1", "prd", "aisdlc-bot", "PRD v1 is attached and ready for review.") // housekeeping
	e.say("PROJ-1", "prd", "dev1", "/ask is CSV enough?")
	e.rec.Tick(context.Background()) // the assistant answers (a ReplyTo comment)
	e.say("PROJ-1", "prd", "asmith", "/changes add export")
	o := only(t, e.rec.Tick(context.Background()))
	if o.Action != ActionFeedback {
		t.Fatalf("%+v", o)
	}
	fb, _ := e.st.LoadFeedback("PROJ-1", "prd", 1)
	var notes, answers int
	for _, c := range fb.Discussion {
		if c.Author == "aisdlc-bot" && c.ReplyTo == "" {
			notes++
		}
		if c.Author == "aisdlc-bot" && c.ReplyTo != "" {
			answers++
		}
	}
	if notes != 0 || answers != 1 || len(fb.Discussion) != 2 { // dev1's question + the assistant's answer
		t.Fatalf("notes=%d answers=%d discussion=%+v", notes, answers, fb.Discussion)
	}
}
