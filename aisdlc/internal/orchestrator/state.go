// Package orchestrator holds the ticket state machine. It is pure (no I/O): callers feed it
// events and persist the result, which makes every transition and cap unit-testable.
package orchestrator

import (
	"fmt"
	"sort"
)

type State string

const (
	New          State = "NEW"
	PRDInterview State = "PRD_INTERVIEW"
	PRDReview    State = "PRD_REVIEW"
	PRDRevision  State = "PRD_REVISION"
	PRDApproved  State = "PRD_APPROVED"
	TechProposal State = "TECH_PROPOSAL"
	TechReview   State = "TECH_REVIEW"
	TechRevision State = "TECH_REVISION"
	TechApproved State = "TECH_APPROVED"
	Building     State = "BUILDING"
	BuildVerify  State = "BUILD_VERIFY"
	AIReview     State = "AI_REVIEW"
	AIFix        State = "AI_FIX"
	ReadyToPush  State = "READY_TO_PUSH"
	PROpen       State = "PR_OPEN"
	PRRework     State = "PR_REWORK" // builder addressing a human code reviewer's requested changes
	Merged       State = "MERGED"
	QARunning    State = "QA_RUNNING"
	QAPassed     State = "QA_PASSED"
	QAFailed     State = "QA_FAILED"
	Done         State = "DONE"
	Escalated    State = "ESCALATED" // waiting for a human
	Failed       State = "FAILED"
	Cancelled    State = "CANCELLED"
)

type Event string

const (
	EvStart          Event = "start"
	EvInterviewDone  Event = "interview_done"
	EvGatePassed     Event = "gate_passed"
	EvFeedback       Event = "feedback"
	EvRevisionReady  Event = "revision_ready"
	EvProceed        Event = "proceed" // auto-advance after an approval / QA pass
	EvTechConfirmed  Event = "tech_confirmed"
	EvBuildDone      Event = "build_done"  // builder exited ok with a non-empty diff
	EvBuildError     Event = "build_error" // builder crashed / timed out / empty diff
	EvVerifyPass     Event = "verify_pass" // orchestrator-run tests, lint, coverage passed
	EvVerifyFail     Event = "verify_fail"
	EvReviewClean    Event = "review_clean"
	EvReviewBlockers Event = "review_blockers"
	EvFixed          Event = "fixed"
	EvPushed         Event = "pushed"
	EvMerged         Event = "merged"
	EvPRDeclined     Event = "pr_declined"
	EvPRChanges      Event = "pr_changes"       // a human code reviewer asked for changes
	EvQATrigger      Event = "qa_trigger"       // manual
	EvQAFixRequested Event = "qa_fix_requested" // QA/dev agreed the QA findings need a code fix
	EvQAPass         Event = "qa_pass"
	EvQAFail         Event = "qa_fail"
	EvEscalate       Event = "escalate"
	EvFail           Event = "fail"
	EvCancel         Event = "cancel"
)

var allEvents = []Event{EvStart, EvInterviewDone, EvGatePassed, EvFeedback, EvRevisionReady, EvProceed,
	EvTechConfirmed, EvBuildDone, EvBuildError, EvVerifyPass, EvVerifyFail, EvReviewClean, EvReviewBlockers,
	EvFixed, EvPushed, EvMerged, EvPRDeclined, EvPRChanges, EvQATrigger, EvQAFixRequested, EvQAPass, EvQAFail, EvEscalate, EvFail, EvCancel}

type Limits struct {
	ReviewRounds  int // max AI review rounds before escalating
	BuildAttempts int // max failed build/verify attempts before escalating
}

// Ctx is the mutable bookkeeping carried alongside the state.
type Ctx struct {
	PRDVersion       int    `json:"prd_version"`
	TechVersion      int    `json:"tech_version"`
	BuildAttempts    int    `json:"build_attempts"`
	ReviewRound      int    `json:"review_round"`
	PRRound          int    `json:"pr_round"` // human code-review rounds (uncapped: people decide when they are done)
	QARun            int    `json:"qa_run"`   // QA runs started (feedback and reports are keyed by it)
	EscalatedFrom    State  `json:"escalated_from,omitempty"`
	EscalationReason string `json:"escalation_reason,omitempty"`
}

type InvalidTransitionError struct {
	State State
	Event Event
}

func (e *InvalidTransitionError) Error() string {
	return fmt.Sprintf("invalid transition: event %q not allowed in state %s", e.Event, e.State)
}

func IsTerminal(s State) bool { return s == Done || s == Failed || s == Cancelled }

func escalate(from State, c Ctx, reason string) (State, Ctx) {
	c.EscalatedFrom, c.EscalationReason = from, reason
	return Escalated, c
}

// Next applies an event. It never mutates its inputs.
func Next(s State, e Event, c Ctx, lim Limits) (State, Ctx, error) {
	bad := func() (State, Ctx, error) { return s, c, &InvalidTransitionError{State: s, Event: e} }

	if IsTerminal(s) {
		return bad()
	}
	// Events valid from any non-terminal state.
	switch e {
	case EvCancel:
		return Cancelled, c, nil
	case EvFail:
		return Failed, c, nil
	case EvEscalate:
		if s == Escalated {
			return bad()
		}
		ns, nc := escalate(s, c, "manual escalation")
		return ns, nc, nil
	}

	// failure budget shared by build errors and failed verification
	budget := func(retry State, why string) (State, Ctx, error) {
		c.BuildAttempts++
		if c.BuildAttempts >= lim.BuildAttempts {
			ns, nc := escalate(s, c, fmt.Sprintf("%s (attempt %d of %d)", why, c.BuildAttempts, lim.BuildAttempts))
			return ns, nc, nil
		}
		return retry, c, nil
	}

	switch s {
	case New:
		if e == EvStart {
			return PRDInterview, c, nil
		}
	case PRDInterview:
		if e == EvInterviewDone {
			if c.PRDVersion == 0 {
				c.PRDVersion = 1
			}
			return PRDReview, c, nil
		}
	case PRDReview:
		switch e {
		case EvGatePassed:
			return PRDApproved, c, nil
		case EvFeedback:
			return PRDRevision, c, nil
		}
	case PRDRevision:
		if e == EvRevisionReady {
			c.PRDVersion++
			return PRDReview, c, nil
		}
	case PRDApproved:
		if e == EvProceed {
			return TechProposal, c, nil
		}
	case TechProposal:
		if e == EvTechConfirmed {
			if c.TechVersion == 0 {
				c.TechVersion = 1
			}
			return TechReview, c, nil
		}
	case TechReview:
		switch e {
		case EvGatePassed:
			return TechApproved, c, nil
		case EvFeedback:
			return TechRevision, c, nil
		}
	case TechRevision:
		if e == EvRevisionReady {
			c.TechVersion++
			return TechReview, c, nil
		}
	case TechApproved:
		if e == EvProceed {
			c.BuildAttempts, c.ReviewRound = 0, 0
			return Building, c, nil
		}
	case Building:
		switch e {
		case EvBuildDone:
			return BuildVerify, c, nil
		case EvBuildError:
			return budget(Building, "builder failed")
		}
	case BuildVerify:
		switch e {
		case EvVerifyPass:
			c.ReviewRound++
			return AIReview, c, nil
		case EvVerifyFail:
			return budget(Building, "tests/lint/coverage failed")
		}
	case AIReview:
		switch e {
		case EvReviewClean:
			return ReadyToPush, c, nil
		case EvReviewBlockers:
			if c.ReviewRound >= lim.ReviewRounds {
				ns, nc := escalate(s, c, fmt.Sprintf("blocking review findings remain after %d rounds", c.ReviewRound))
				return ns, nc, nil
			}
			return AIFix, c, nil
		}
	case AIFix:
		switch e {
		case EvFixed:
			return BuildVerify, c, nil
		case EvBuildError:
			return budget(AIFix, "builder failed while fixing review findings")
		}
	case ReadyToPush:
		if e == EvPushed {
			return PROpen, c, nil
		}
	case PROpen:
		switch e {
		case EvMerged:
			return Merged, c, nil
		case EvPRDeclined:
			ns, nc := escalate(s, c, "pull request was declined")
			return ns, nc, nil
		case EvPRChanges:
			// A human review round is never capped. The agent-vs-agent budgets start fresh.
			c.PRRound++
			c.ReviewRound, c.BuildAttempts = 0, 0
			return PRRework, c, nil
		}
	case PRRework:
		switch e {
		case EvFixed:
			return BuildVerify, c, nil // tests, then a fresh AI review, then a push to the same PR
		case EvBuildError:
			return budget(PRRework, "builder failed while addressing PR review comments")
		}
	case Merged:
		if e == EvQATrigger {
			c.QARun++
			return QARunning, c, nil
		}
	case QARunning:
		switch e {
		case EvQAPass:
			return QAPassed, c, nil
		case EvQAFail:
			return QAFailed, c, nil
		}
	case QAPassed:
		if e == EvProceed {
			return Done, c, nil
		}
	case QAFailed:
		switch e {
		case EvQATrigger: // re-run after a fix lands on main, or to confirm a flaky failure
			c.QARun++
			return QARunning, c, nil
		case EvQAFixRequested: // findings go back to the developer: a new branch, review, PR, merge
			c.BuildAttempts, c.ReviewRound = 0, 0
			return Building, c, nil
		}
	}
	return bad()
}

// resumeTargets are the only states a human may send an escalated ticket back to.
var resumeTargets = map[State]bool{PRDRevision: true, TechRevision: true, Building: true, PRRework: true, AIReview: true, ReadyToPush: true}

// Resume moves an ESCALATED ticket to a chosen state, resetting the relevant budget so the
// human's decision buys a fresh set of attempts.
func Resume(s State, c Ctx, to State) (State, Ctx, error) {
	if s != Escalated {
		return s, c, fmt.Errorf("resume: ticket is %s, not %s", s, Escalated)
	}
	if !resumeTargets[to] {
		return s, c, fmt.Errorf("resume: cannot resume to %s", to)
	}
	switch to {
	case Building, PRRework:
		c.BuildAttempts = 0
	case AIReview:
		c.ReviewRound = 0
	}
	c.EscalatedFrom, c.EscalationReason = "", ""
	return to, c, nil
}

// Allowed lists the events that are valid in state s (useful for status output).
func Allowed(s State, c Ctx, lim Limits) []Event {
	var out []Event
	for _, e := range allEvents {
		if _, _, err := Next(s, e, c, lim); err == nil {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
