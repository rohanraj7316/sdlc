package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"

	"aisdlc/internal/orchestrator"
	"aisdlc/internal/store/storetest"
)

var lim = orchestrator.Limits{ReviewRounds: 2, BuildAttempts: 2}

// open returns a store on a fresh schema, plus its DSN so a test can open a second store
// (standing in for another process) on the same data.
func open(t *testing.T) (*Store, string) {
	t.Helper()
	dsn := storetest.DSN(t)
	return openDSN(t, dsn), dsn
}

func openDSN(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestKeyValidationBlocksTraversal(t *testing.T) {
	s, _ := open(t)
	for _, k := range []string{"../etc/passwd", "proj-1", "PROJ-", "PROJ-1/../x", "", "PROJ 1", "PROJ-1.json"} {
		if _, err := s.Load(k); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Load(%q) must be rejected as invalid, got %v", k, err)
		}
		if _, err := s.Apply(k, orchestrator.EvStart, "", lim); err == nil {
			t.Errorf("Apply(%q) must be rejected", k)
		}
		if _, err := s.AddArtifact(k, "prd", []byte("x")); err == nil {
			t.Errorf("AddArtifact(%q) must be rejected", k)
		}
	}
	if _, err := s.Load("PROJ-123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("valid but missing key should be ErrNotFound, got %v", err)
	}
}

func TestApplyPersistsAcrossReopen(t *testing.T) {
	s, dsn := open(t)
	if _, err := s.Apply("PROJ-1", orchestrator.EvStart, "kickoff", lim); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply("PROJ-1", orchestrator.EvInterviewDone, "", lim); err != nil {
		t.Fatal(err)
	}
	tk, err := openDSN(t, dsn).Load("PROJ-1")
	if err != nil {
		t.Fatal(err)
	}
	if tk.State != orchestrator.PRDReview || tk.Ctx.PRDVersion != 1 || len(tk.History) != 2 {
		t.Fatalf("%+v", tk)
	}
	if h := tk.History[0]; h.From != orchestrator.New || h.To != orchestrator.PRDInterview || h.Note != "kickoff" {
		t.Fatalf("history[0] = %+v", h)
	}
}

func TestInvalidTransitionWritesNothing(t *testing.T) {
	s, _ := open(t)
	s.Apply("PROJ-2", orchestrator.EvStart, "", lim)
	before, _ := s.Load("PROJ-2")
	_, err := s.Apply("PROJ-2", orchestrator.EvMerged, "", lim)
	var ite *orchestrator.InvalidTransitionError
	if !errors.As(err, &ite) {
		t.Fatalf("want InvalidTransitionError, got %v", err)
	}
	after, _ := s.Load("PROJ-2")
	if after.State != before.State || len(after.History) != len(before.History) {
		t.Fatal("a rejected transition must not change persisted state")
	}
	// a brand-new ticket hit with an invalid event must not be created
	if _, err := s.Apply("PROJ-3", orchestrator.EvMerged, "", lim); err == nil {
		t.Fatal("expected error")
	}
	if _, err := s.Load("PROJ-3"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PROJ-3 must not exist, got %v", err)
	}
}

func TestAddArtifactVersioningAndHash(t *testing.T) {
	s, _ := open(t)
	a1, err := s.AddArtifact("PROJ-4", "prd", []byte("v1 body"))
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := s.AddArtifact("PROJ-4", "prd", []byte("v2 body"))
	tech, _ := s.AddArtifact("PROJ-4", "tech", []byte("plan"))
	if a1.Version != 1 || a2.Version != 2 || tech.Version != 1 {
		t.Fatalf("versions: %d %d %d", a1.Version, a2.Version, tech.Version)
	}
	sum := sha256.Sum256([]byte("v2 body"))
	if a2.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("sha256 mismatch")
	}
	b, err := s.ArtifactContent("PROJ-4", "prd", 2)
	if err != nil || string(b) != "v2 body" {
		t.Fatalf("artifact content wrong: %q %v", b, err)
	}
	if _, err := s.ArtifactContent("PROJ-4", "prd", 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing version should be ErrNotFound, got %v", err)
	}
	tk, _ := s.Load("PROJ-4")
	if latest, ok := tk.LatestArtifact("prd"); !ok || latest.Version != 2 {
		t.Fatalf("latest = %+v", latest)
	}
	if _, ok := tk.LatestArtifact("qa"); ok {
		t.Fatal("no qa artifact expected")
	}
	if _, err := s.AddArtifact("PROJ-4", "../x", []byte("x")); err == nil {
		t.Fatal("bad kind must be rejected")
	}
}

func TestConcurrentApplyIsSerialised(t *testing.T) {
	s, _ := open(t)
	s.Apply("PROJ-6", orchestrator.EvStart, "", lim)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Apply("PROJ-6", orchestrator.EvInterviewDone, "", lim); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("exactly one concurrent transition may win, got %d", ok)
	}
	tk, _ := s.Load("PROJ-6")
	if len(tk.History) != 2 {
		t.Fatalf("history = %d", len(tk.History))
	}
}

func TestListAndEventLog(t *testing.T) {
	s, _ := open(t)
	s.Apply("PROJ-9", orchestrator.EvStart, "", lim)
	s.Apply("ABC-1", orchestrator.EvStart, "", lim)
	keys, _ := s.List()
	if strings.Join(keys, ",") != "ABC-1,PROJ-9" {
		t.Fatalf("keys = %v", keys)
	}
	if err := s.LogEvent("PROJ-9", "agent_run", map[string]any{"role": "builder"}); err != nil {
		t.Fatal(err)
	}
	s.LogEvent("PROJ-9", "agent_run", "second")
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM events WHERE key = 'PROJ-9' AND kind = 'agent_run'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("want 2 events, got %d (%v)", n, err)
	}
}
