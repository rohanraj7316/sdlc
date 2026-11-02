package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"aisdlc/internal/gates"
	"aisdlc/internal/orchestrator"
	"aisdlc/internal/store/storetest"
)

var dLim = orchestrator.Limits{ReviewRounds: 2, BuildAttempts: 2}

func TestApplyExpectIsCompareAndSet(t *testing.T) {
	st, _ := open(t)
	st.Apply("PROJ-1", orchestrator.EvStart, "", dLim)
	st.AddArtifact("PROJ-1", "prd", []byte("v1"))
	st.Apply("PROJ-1", orchestrator.EvInterviewDone, "", dLim) // PRD_REVIEW, PRD v1

	if _, err := st.ApplyExpect("PROJ-1", orchestrator.EvGatePassed, "", dLim, Expect{State: orchestrator.PRDReview, PRDVersion: 2}); !errors.Is(err, ErrStale) {
		t.Fatalf("wrong version must be stale, got %v", err)
	}
	if _, err := st.ApplyExpect("PROJ-1", orchestrator.EvGatePassed, "", dLim, Expect{State: orchestrator.TechReview}); !errors.Is(err, ErrStale) {
		t.Fatalf("wrong state must be stale, got %v", err)
	}
	tk, _ := st.Load("PROJ-1")
	if tk.State != orchestrator.PRDReview || len(tk.History) != 2 {
		t.Fatal("stale attempts must not write anything")
	}
	if _, err := st.ApplyExpect("PROJ-1", orchestrator.EvGatePassed, "", dLim, Expect{State: orchestrator.PRDReview, PRDVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if tk, _ = st.Load("PROJ-1"); tk.State != orchestrator.PRDApproved {
		t.Fatalf("state = %s", tk.State)
	}
}

// Separate Store values stand in for separate processes: the row lock must make their
// read-modify-write cycles exclusive.
func TestWritersInDifferentStoreInstancesDoNotLoseUpdates(t *testing.T) {
	dsn := storetest.DSN(t)
	const n = 12
	var wg sync.WaitGroup
	vers := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := Open(dsn)
			if err != nil {
				t.Error(err)
				return
			}
			defer st.Close()
			a, err := st.AddArtifact("PROJ-1", "prd", []byte("x"))
			if err != nil {
				t.Error(err)
				return
			}
			vers <- a.Version
		}()
	}
	wg.Wait()
	close(vers)
	seen := map[int]bool{}
	for v := range vers {
		if seen[v] {
			t.Fatalf("version %d handed out twice: lost update", v)
		}
		seen[v] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d distinct versions, want %d", len(seen), n)
	}
	st := openDSN(t, dsn)
	tk, _ := st.Load("PROJ-1")
	if len(tk.Artifacts) != n {
		t.Fatalf("ticket records %d artifacts, want %d", len(tk.Artifacts), n)
	}
}

func TestFeedbackRoundTrip(t *testing.T) {
	st, _ := open(t)
	if _, err := st.LoadFeedback("PROJ-1", "prd", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	in := Feedback{
		Changes:    []gates.Comment{{ID: "1", Author: "asmith", Created: time.Unix(1700000000, 0).UTC(), Body: "/changes add export"}},
		Discussion: []gates.Comment{{ID: "2", Author: "dev1", Created: time.Unix(1700000100, 0).UTC(), Body: "easy to do"}},
		Roles:      map[string]string{"asmith": "product", "dev1": "developer"},
	}
	if _, err := st.SaveFeedback("PROJ-1", "prd", 1, in); err != nil {
		t.Fatal(err)
	}
	out, err := st.LoadFeedback("PROJ-1", "prd", 1)
	if err != nil || len(out.Changes) != 1 || len(out.Discussion) != 1 || out.Roles["dev1"] != "developer" || !out.Changes[0].Created.Equal(in.Changes[0].Created) {
		t.Fatalf("round trip: %+v %v", out, err)
	}
	for _, kind := range []string{"pr", "qa", "tech"} { // every thread keeps its own rounds
		if _, err := st.SaveFeedback("PROJ-1", kind, 2, in); err != nil {
			t.Fatalf("kind %s: %v", kind, err)
		}
	}
	if _, err := st.SaveFeedback("PROJ-1", "../x", 1, in); err == nil {
		t.Fatal("kind must be validated on save")
	}
	if _, err := st.LoadFeedback("PROJ-1", "../x", 1); err == nil {
		t.Fatal("kind must be validated on load")
	}
}

// A question must be answered at most once even when several reconcilers see it at the same time.
func TestClaimReplyHasExactlyOneWinner(t *testing.T) {
	dsn := storetest.DSN(t)
	const n = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := Open(dsn)
			if err != nil {
				t.Error(err)
				return
			}
			defer st.Close()
			won, err := st.ClaimReply("PROJ-1", "prd:c7")
			if err != nil {
				t.Error(err)
				return
			}
			if won {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("%d winners, want exactly 1", winners)
	}
	st := openDSN(t, dsn)
	if won, _ := st.ClaimReply("PROJ-1", "prd:c7"); won {
		t.Fatal("already claimed")
	}
	if won, _ := st.ClaimReply("PROJ-1", "tech:c7"); !won {
		t.Fatal("comment ids are per thread: tech:c7 is a different question")
	}
	// a failed attempt gives the claim back so the next pass retries
	if err := st.ReleaseReply("PROJ-1", "prd:c7"); err != nil {
		t.Fatal(err)
	}
	if won, _ := st.ClaimReply("PROJ-1", "prd:c7"); !won {
		t.Fatal("released claim must be claimable again")
	}
}

func TestApplyExpectPinsPRRoundAndQARun(t *testing.T) {
	st, _ := open(t)
	for _, ev := range []orchestrator.Event{orchestrator.EvStart, orchestrator.EvInterviewDone, orchestrator.EvGatePassed, orchestrator.EvProceed,
		orchestrator.EvTechConfirmed, orchestrator.EvGatePassed, orchestrator.EvProceed, orchestrator.EvBuildDone, orchestrator.EvVerifyPass,
		orchestrator.EvReviewClean, orchestrator.EvPushed} {
		if _, err := st.Apply("PROJ-1", ev, "", dLim); err != nil {
			t.Fatalf("%s: %v", ev, err)
		}
	}
	// PR_OPEN, round 0. A decision made for round 1 must not apply.
	if _, err := st.ApplyExpect("PROJ-1", orchestrator.EvPRChanges, "", dLim, Expect{State: orchestrator.PROpen, PRRound: 1}); !errors.Is(err, ErrStale) {
		t.Fatalf("want stale, got %v", err)
	}
	if _, err := st.ApplyExpect("PROJ-1", orchestrator.EvPRChanges, "", dLim, Expect{State: orchestrator.PROpen}); err != nil {
		t.Fatal(err)
	}
	tk, _ := st.Load("PROJ-1")
	if tk.State != orchestrator.PRRework || tk.Ctx.PRRound != 1 {
		t.Fatalf("%s %+v", tk.State, tk.Ctx)
	}
}

func TestInStateSince(t *testing.T) {
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	st, _ := OpenWithClock(storetest.DSN(t), func() time.Time { return now })
	st.Apply("PROJ-1", orchestrator.EvStart, "", dLim)
	now = now.Add(time.Hour)
	st.AddArtifact("PROJ-1", "prd", []byte("x"))
	st.Apply("PROJ-1", orchestrator.EvInterviewDone, "", dLim)
	reviewAt := now
	now = now.Add(72 * time.Hour)
	tk, _ := st.Load("PROJ-1")
	if !tk.InStateSince().Equal(reviewAt) {
		t.Fatalf("since = %v, want %v", tk.InStateSince(), reviewAt)
	}
}
