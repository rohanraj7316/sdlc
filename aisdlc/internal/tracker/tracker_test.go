package tracker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aisdlc/internal/gates"
)

func TestDirTracker(t *testing.T) {
	dir := t.TempDir()
	d := DirTracker{Dir: dir}
	ctx := context.Background()

	if cs, err := d.Comments(ctx, "PROJ-1", ThreadPRD); err != nil || len(cs) != 0 {
		t.Fatalf("no file yet must be an empty thread, got %v %v", cs, err)
	}
	os.MkdirAll(filepath.Join(dir, "PROJ-1"), 0o755)
	os.WriteFile(filepath.Join(dir, "PROJ-1", "prd.json"),
		[]byte(`[{"id":"1","author":"jdoe","created":"2026-10-05T10:00:00Z","body":"approved"}]`), 0o644)
	cs, err := d.Comments(ctx, "PROJ-1", ThreadPRD)
	if err != nil || len(cs) != 1 || cs[0].Author != "jdoe" {
		t.Fatalf("got %v %v", cs, err)
	}
	os.WriteFile(filepath.Join(dir, "PROJ-1", "tech.json"), []byte(`{not json`), 0o644)
	if _, err := d.Comments(ctx, "PROJ-1", ThreadTech); err == nil {
		t.Fatal("malformed file must be an error (callers treat it as transient)")
	}
	if _, err := d.Comments(ctx, "PROJ-1", Thread("../etc")); err == nil {
		t.Fatal("unknown thread must be rejected")
	}
}

func TestDirTrackerPostAndAllThreads(t *testing.T) {
	d := DirTracker{Dir: t.TempDir()}
	ctx := context.Background()
	for _, th := range []Thread{ThreadPRD, ThreadTech, ThreadPR, ThreadQA} {
		c := gates.Comment{ID: "b1", Author: "aisdlc-bot", Created: time.Now().UTC(), Body: "hello " + string(th), ReplyTo: "c1"}
		if err := d.Post(ctx, "PROJ-1", th, c); err != nil {
			t.Fatalf("%s: %v", th, err)
		}
		if err := d.Post(ctx, "PROJ-1", th, gates.Comment{ID: "b2", Author: "aisdlc-bot", Created: time.Now().UTC(), Body: "second"}); err != nil {
			t.Fatal(err)
		}
		cs, err := d.Comments(ctx, "PROJ-1", th)
		if err != nil || len(cs) != 2 || cs[0].ReplyTo != "c1" || cs[1].ID != "b2" {
			t.Fatalf("%s: %+v %v", th, cs, err)
		}
	}
	if err := d.Post(ctx, "PROJ-1", Thread("nope"), gates.Comment{}); err == nil {
		t.Fatal("unknown thread must be rejected")
	}
}

func TestDirTrackerPRState(t *testing.T) {
	dir := t.TempDir()
	d := DirTracker{Dir: dir}
	ctx := context.Background()
	if st, err := d.PRState(ctx, "PROJ-1"); err != nil || st != PROpen {
		t.Fatalf("missing file means open: %v %v", st, err)
	}
	os.MkdirAll(filepath.Join(dir, "PROJ-1"), 0o755)
	for in, want := range map[string]PRState{"merged\n": PRMerged, " Declined ": PRDeclined, "OPEN": PROpen} {
		os.WriteFile(filepath.Join(dir, "PROJ-1", "pr_state"), []byte(in), 0o644)
		if st, err := d.PRState(ctx, "PROJ-1"); err != nil || st != want {
			t.Fatalf("%q -> %v %v", in, st, err)
		}
	}
	os.WriteFile(filepath.Join(dir, "PROJ-1", "pr_state"), []byte("approved"), 0o644)
	if _, err := d.PRState(ctx, "PROJ-1"); err == nil {
		t.Fatal("unknown state must be an error")
	}
	var _ Poster = d
	var _ PRWatcher = d
}
