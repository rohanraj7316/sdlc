package orchestrator

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

var lim = Limits{ReviewRounds: 2, BuildAttempts: 2}

// walk applies events in order from `from`, failing the test on any invalid step.
func walk(t *testing.T, from State, c Ctx, steps ...Event) (State, Ctx) {
	t.Helper()
	s := from
	for i, e := range steps {
		var err error
		s, c, err = Next(s, e, c, lim)
		if err != nil {
			t.Fatalf("step %d (%s): %v", i, e, err)
		}
	}
	return s, c
}

func TestHappyPath(t *testing.T) {
	s, c := walk(t, New, Ctx{},
		EvStart, EvInterviewDone, EvGatePassed, EvProceed, EvTechConfirmed, EvGatePassed, EvProceed,
		EvBuildDone, EvVerifyPass, EvReviewClean, EvPushed, EvMerged, EvQATrigger, EvQAPass, EvProceed)
	if s != Done {
		t.Fatalf("ended in %s", s)
	}
	if c.PRDVersion != 1 || c.TechVersion != 1 || c.ReviewRound != 1 {
		t.Fatalf("ctx = %+v", c)
	}
}

func TestRevisionLoopsBumpVersions(t *testing.T) {
	s, c := walk(t, New, Ctx{}, EvStart, EvInterviewDone, EvFeedback, EvRevisionReady, EvFeedback, EvRevisionReady)
	if s != PRDReview || c.PRDVersion != 3 {
		t.Fatalf("PRD: %s %+v", s, c)
	}
	s, c = walk(t, s, c, EvGatePassed, EvProceed, EvTechConfirmed, EvFeedback, EvRevisionReady)
	if s != TechReview || c.TechVersion != 2 {
		t.Fatalf("TECH: %s %+v", s, c)
	}
}

func TestReviewRoundsCapEscalates(t *testing.T) {
	// round 1: blockers -> fix -> verify -> round 2: blockers -> escalate
	s, c := walk(t, TechApproved, Ctx{}, EvProceed, EvBuildDone, EvVerifyPass, EvReviewBlockers)
	if s != AIFix || c.ReviewRound != 1 {
		t.Fatalf("after round 1: %s %+v", s, c)
	}
	s, c = walk(t, s, c, EvFixed, EvVerifyPass, EvReviewBlockers)
	if s != Escalated || c.EscalatedFrom != AIReview || !strings.Contains(c.EscalationReason, "2 rounds") {
		t.Fatalf("after round 2: %s %+v", s, c)
	}
}

func TestReviewCleanOnSecondRound(t *testing.T) {
	s, _ := walk(t, TechApproved, Ctx{}, EvProceed, EvBuildDone, EvVerifyPass, EvReviewBlockers, EvFixed, EvVerifyPass, EvReviewClean)
	if s != ReadyToPush {
		t.Fatalf("got %s", s)
	}
}

func TestBuildBudgetEscalates(t *testing.T) {
	// first failure retries, second escalates (BuildAttempts = 2)
	s, c := walk(t, Building, Ctx{}, EvBuildError)
	if s != Building || c.BuildAttempts != 1 {
		t.Fatalf("%s %+v", s, c)
	}
	s, c = walk(t, s, c, EvBuildError)
	if s != Escalated || c.EscalatedFrom != Building {
		t.Fatalf("%s %+v", s, c)
	}
	// failed verification shares the same budget
	s, c = walk(t, Building, Ctx{}, EvBuildDone, EvVerifyFail)
	if s != Building || c.BuildAttempts != 1 {
		t.Fatalf("%s %+v", s, c)
	}
	s, _ = walk(t, s, c, EvBuildDone, EvVerifyFail)
	if s != Escalated {
		t.Fatalf("shared budget not enforced: %s", s)
	}
	// builder failing while fixing review findings also counts
	s, c = walk(t, AIFix, Ctx{}, EvBuildError)
	if s != AIFix || c.BuildAttempts != 1 {
		t.Fatalf("%s %+v", s, c)
	}
}

func TestResume(t *testing.T) {
	_, c := walk(t, Building, Ctx{}, EvBuildError, EvBuildError)
	s, c, err := Resume(Escalated, c, Building)
	if err != nil || s != Building || c.BuildAttempts != 0 || c.EscalatedFrom != "" || c.EscalationReason != "" {
		t.Fatalf("%s %+v %v", s, c, err)
	}
	// a fresh budget really works: one failure no longer escalates
	if s, _ = walk(t, s, c, EvBuildError); s != Building {
		t.Fatalf("got %s", s)
	}
	if _, _, err := Resume(Building, Ctx{}, Building); err == nil {
		t.Fatal("must not resume a ticket that is not escalated")
	}
	if _, _, err := Resume(Escalated, Ctx{}, Done); err == nil {
		t.Fatal("must not resume straight to DONE")
	}
	if s, c, err := Resume(Escalated, Ctx{ReviewRound: 2}, AIReview); err != nil || s != AIReview || c.ReviewRound != 0 {
		t.Fatalf("%s %+v %v", s, c, err)
	}
}

func TestInvalidTransitions(t *testing.T) {
	bad := []struct {
		s State
		e Event
	}{
		{New, EvGatePassed}, {PRDReview, EvProceed}, {PRDInterview, EvGatePassed},
		{TechApproved, EvBuildDone}, {ReadyToPush, EvMerged}, {PROpen, EvQATrigger},
		{Merged, EvQAPass}, {Escalated, EvProceed}, {Escalated, EvEscalate},
		{Done, EvStart}, {Failed, EvCancel}, {Cancelled, EvFail},
	}
	for _, b := range bad {
		_, _, err := Next(b.s, b.e, Ctx{}, lim)
		var ite *InvalidTransitionError
		if !errors.As(err, &ite) {
			t.Errorf("%s + %s: expected InvalidTransitionError, got %v", b.s, b.e, err)
		}
	}
}

func TestCancelFailEscalateFromAnyActiveState(t *testing.T) {
	for _, s := range []State{New, PRDReview, TechReview, Building, AIReview, PROpen, QARunning, Escalated} {
		if ns, _, err := Next(s, EvCancel, Ctx{}, lim); err != nil || ns != Cancelled {
			t.Errorf("%s: cancel -> %s, %v", s, ns, err)
		}
		if ns, _, err := Next(s, EvFail, Ctx{}, lim); err != nil || ns != Failed {
			t.Errorf("%s: fail -> %s, %v", s, ns, err)
		}
	}
	if ns, c, err := Next(PROpen, EvEscalate, Ctx{}, lim); err != nil || ns != Escalated || c.EscalatedFrom != PROpen {
		t.Errorf("escalate: %s %+v %v", ns, c, err)
	}
}

func TestPRDeclinedEscalates(t *testing.T) {
	s, c, err := Next(PROpen, EvPRDeclined, Ctx{}, lim)
	if err != nil || s != Escalated || !strings.Contains(c.EscalationReason, "declined") {
		t.Fatalf("%s %+v %v", s, c, err)
	}
}

func TestQARerunAfterFailure(t *testing.T) {
	s, _ := walk(t, Merged, Ctx{}, EvQATrigger, EvQAFail, EvQATrigger, EvQAPass, EvProceed)
	if s != Done {
		t.Fatalf("got %s", s)
	}
}

func TestNextDoesNotMutateInput(t *testing.T) {
	c := Ctx{BuildAttempts: 0}
	Next(Building, EvBuildError, c, lim)
	if c.BuildAttempts != 0 {
		t.Fatal("Next mutated its input Ctx")
	}
}

func TestAllowed(t *testing.T) {
	got := Allowed(PRDReview, Ctx{}, lim)
	want := []Event{EvCancel, EvEscalate, EvFail, EvFeedback, EvGatePassed}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Allowed(PRDReview) = %v, want %v", got, want)
	}
	if got := Allowed(Done, Ctx{}, lim); len(got) != 0 {
		t.Fatalf("terminal state should allow nothing, got %v", got)
	}
}

func TestHumanCodeReviewLoopIsUncappedAndResetsAgentBudgets(t *testing.T) {
	// first push, then FOUR rounds of human review: each goes rework -> verify -> fresh AI review -> push
	s, c := walk(t, TechApproved, Ctx{}, EvProceed, EvBuildDone, EvVerifyPass, EvReviewBlockers, EvFixed, EvVerifyPass, EvReviewClean, EvPushed)
	if s != PROpen || c.PRRound != 0 {
		t.Fatalf("setup: %s %+v", s, c)
	}
	for round := 1; round <= 4; round++ {
		s, c = walk(t, s, c, EvPRChanges)
		if s != PRRework || c.PRRound != round || c.ReviewRound != 0 || c.BuildAttempts != 0 {
			t.Fatalf("round %d entry: %s %+v (agent budgets must start fresh)", round, s, c)
		}
		s, c = walk(t, s, c, EvFixed, EvVerifyPass, EvReviewClean, EvPushed)
		if s != PROpen || c.ReviewRound != 1 {
			t.Fatalf("round %d exit: %s %+v", round, s, c)
		}
	}
	if s, _ = walk(t, s, c, EvMerged); s != Merged {
		t.Fatalf("got %s", s)
	}
}

func TestPRReworkFailureBudgetEscalates(t *testing.T) {
	s, c := walk(t, PROpen, Ctx{}, EvPRChanges, EvBuildError)
	if s != PRRework || c.BuildAttempts != 1 {
		t.Fatalf("%s %+v", s, c)
	}
	s, c = walk(t, s, c, EvBuildError)
	if s != Escalated || c.EscalatedFrom != PRRework {
		t.Fatalf("%s %+v", s, c)
	}
	if s, c, err := Resume(s, c, PRRework); err != nil || s != PRRework || c.BuildAttempts != 0 {
		t.Fatalf("resume to rework: %s %+v %v", s, c, err)
	}
}

func TestQAFailureLoopsBackThroughDevAndCountsRuns(t *testing.T) {
	s, c := walk(t, Merged, Ctx{}, EvQATrigger, EvQAFail)
	if s != QAFailed || c.QARun != 1 {
		t.Fatalf("%s %+v", s, c)
	}
	// QA and the developer agree it needs a code fix: a full build/review/PR/merge cycle follows
	s, c = walk(t, s, c, EvQAFixRequested)
	if s != Building || c.BuildAttempts != 0 || c.ReviewRound != 0 {
		t.Fatalf("%s %+v", s, c)
	}
	s, c = walk(t, s, c, EvBuildDone, EvVerifyPass, EvReviewClean, EvPushed, EvMerged, EvQATrigger, EvQAPass, EvProceed)
	if s != Done || c.QARun != 2 {
		t.Fatalf("%s %+v", s, c)
	}
	// or they just want a retest without a fix
	s, c = walk(t, QAFailed, Ctx{QARun: 1}, EvQATrigger)
	if s != QARunning || c.QARun != 2 {
		t.Fatalf("%s %+v", s, c)
	}
}

func TestNewTransitionsRejectWrongStates(t *testing.T) {
	for _, b := range []struct {
		s State
		e Event
	}{{PRDReview, EvPRChanges}, {PRRework, EvMerged}, {PRRework, EvPushed}, {Merged, EvQAFixRequested}, {QARunning, EvQAFixRequested}, {QAPassed, EvQAFixRequested}} {
		if _, _, err := Next(b.s, b.e, Ctx{}, lim); err == nil {
			t.Errorf("%s + %s must be invalid", b.s, b.e)
		}
	}
	if got := Allowed(PROpen, Ctx{}, lim); !reflect.DeepEqual(got, []Event{EvCancel, EvEscalate, EvFail, EvMerged, EvPRChanges, EvPRDeclined}) {
		t.Fatalf("Allowed(PROpen) = %v", got)
	}
}
