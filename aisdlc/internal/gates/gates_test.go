package gates

import (
	"testing"
	"time"
)

func TestParseApproval(t *testing.T) {
	cases := []struct {
		in      string
		ok      bool
		version int
	}{
		{"approved", true, 0},
		{"Approve", true, 0},
		{"/approve", true, 0},
		{"LGTM", true, 0},
		{"lgtm!", true, 0},
		{"Good to go", true, 0},
		{"good to go from my side", true, 0},
		{"Done.", true, 0},
		{"okay, done", true, 0}, // a leading ok/okay is fine...
		{"ok good to go from my side", true, 0},
		{"okay, done, but fix the scope", false, 0}, // ...but the phrase must still fill the whole line
		{"okay", false, 0},
		{"signed off", true, 0},
		{"sign-off", true, 0},
		{"approved v3", true, 3},
		{"approve v12!", true, 12},
		{"  \n\napproved\nthanks for the quick turnaround", true, 0}, // first non-empty line only
		{"not approved", false, 0},
		{"done, but please fix the scope section", false, 0},
		{"approved with changes", false, 0},
		{"I approve", false, 0},
		{"disapproved", false, 0},
		{"can we discuss before I approve?", false, 0},
		{"", false, 0},
	}
	for _, c := range cases {
		ok, v := ParseApproval(c.in)
		if ok != c.ok || v != c.version {
			t.Errorf("ParseApproval(%q) = (%v,%d), want (%v,%d)", c.in, ok, v, c.ok, c.version)
		}
	}
}

var t0 = time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)

func at(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

func cm(id, author string, min int, body string) Comment {
	return Comment{ID: id, Author: author, Created: at(min), Body: body}
}

func TestEvaluate(t *testing.T) {
	cfg := Config{Approvers: []string{"jdoe", "asmith"}, Quorum: QuorumAll, Version: 2, UploadedAt: at(0)}

	t.Run("all approvers approve", func(t *testing.T) {
		r := Evaluate(cfg, []Comment{cm("1", "jdoe", 5, "approved"), cm("2", "asmith", 6, "good to go from my side")})
		if !r.Passed || len(r.Approvals) != 2 || len(r.Pending) != 0 {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("one pending blocks quorum all", func(t *testing.T) {
		r := Evaluate(cfg, []Comment{cm("1", "jdoe", 5, "approved")})
		if r.Passed || len(r.Pending) != 1 || r.Pending[0] != "asmith" {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("quorum any", func(t *testing.T) {
		c := cfg
		c.Quorum = QuorumAny
		if r := Evaluate(c, []Comment{cm("1", "jdoe", 5, "lgtm")}); !r.Passed {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("non-approvers can talk but never approve or request changes", func(t *testing.T) {
		r := Evaluate(cfg, []Comment{cm("1", "stranger", 5, "approved"), cm("2", "stranger", 6, "/changes rewrite it all")})
		if r.Passed || len(r.Approvals) != 0 || len(r.Changes) != 0 || len(r.Discussion) != 2 {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("approval before the upload does not count (revision resets)", func(t *testing.T) {
		r := Evaluate(cfg, []Comment{cm("1", "jdoe", -10, "approved"), cm("2", "asmith", -5, "approved")})
		if r.Passed || len(r.Stale) != 2 {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("approval naming an older version is stale", func(t *testing.T) {
		r := Evaluate(cfg, []Comment{cm("1", "jdoe", 5, "approved v1"), cm("2", "asmith", 6, "approved v2")})
		if r.Passed || len(r.Stale) != 1 || len(r.Approvals) != 1 || r.Approvals[0].Approver != "asmith" {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("/changes is collected and withdraws an earlier approval by that approver", func(t *testing.T) {
		r := Evaluate(cfg, []Comment{
			cm("1", "jdoe", 5, "approved"), cm("2", "asmith", 6, "approved"),
			cm("3", "jdoe", 7, "/changes the rollout section is missing a rollback plan"),
		})
		if r.Passed || len(r.Changes) != 1 || r.Changes[0].ID != "3" || len(r.Pending) != 1 || r.Pending[0] != "jdoe" {
			t.Fatalf("%+v", r)
		}
		// ...and re-approving afterwards restores it
		r = Evaluate(cfg, []Comment{
			cm("1", "jdoe", 5, "approved"), cm("2", "asmith", 6, "approved"),
			cm("3", "jdoe", 7, "/changes missing rollback plan"), cm("4", "jdoe", 8, "approved"),
		})
		if !r.Passed {
			t.Fatalf("re-approval should pass: %+v", r)
		}
	})

	t.Run("order of input does not matter; author match is case-insensitive", func(t *testing.T) {
		r := Evaluate(cfg, []Comment{cm("2", "ASmith", 6, "approved"), cm("1", "JDoe", 5, "approved")})
		if !r.Passed {
			t.Fatalf("%+v", r)
		}
	})

	t.Run("no approvers configured never passes", func(t *testing.T) {
		if Evaluate(Config{Quorum: QuorumAll, UploadedAt: at(0)}, []Comment{cm("1", "x", 5, "approved")}).Passed {
			t.Fatal("empty approver list must not pass")
		}
	})

	t.Run("deleting an approval reopens the gate on the next evaluation", func(t *testing.T) {
		both := []Comment{cm("1", "jdoe", 5, "approved"), cm("2", "asmith", 6, "approved")}
		if !Evaluate(cfg, both).Passed {
			t.Fatal("setup")
		}
		if Evaluate(cfg, both[:1]).Passed {
			t.Fatal("gate must reopen when an approval disappears")
		}
	})
}

func TestParseCommand(t *testing.T) {
	cases := map[string]string{
		"/changes please add export": "changes", "/CHANGES": "changes", "  /ask what does Out mean?": "ask",
		"/fix": "fix", "/qa": "qa", "\n\n/changes later": "changes",
		"changes please": "", "/changesx": "", "can you /ask?": "", "": "", "/approve": "", "/unknown": "",
	}
	for in, want := range cases {
		if got := ParseCommand(in); got != want {
			t.Errorf("ParseCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

// The point of explicit intent: talking is free, and a question is never a rewrite request.
func TestConversationNeverMovesTheGateByItself(t *testing.T) {
	cfg := Config{Approvers: []string{"jdoe", "asmith"}, Quorum: QuorumAll, Version: 1, UploadedAt: at(0)}
	r := Evaluate(cfg, []Comment{
		cm("1", "jdoe", 5, "approved"),
		cm("2", "jdoe", 6, "can we clarify what is out of scope?"), // an approver's question
		cm("3", "dev1", 7, "technically the export is easy"),       // a developer
		cm("4", "qa1", 8, "/ask what is the expected export format?"),
		cm("5", "asmith", 9, "done, but I am not sure about metrics"), // not a clean approval, not a command
	})
	if len(r.Changes) != 0 {
		t.Fatalf("nothing here is a change request: %+v", r.Changes)
	}
	if len(r.Approvals) != 1 || r.Approvals[0].Approver != "jdoe" {
		t.Fatalf("jdoe's approval must survive his own question: %+v", r.Approvals)
	}
	if r.Passed || len(r.Pending) != 1 || r.Pending[0] != "asmith" {
		t.Fatalf("asmith has not approved: %+v", r)
	}
	if len(r.Discussion) != 4 || len(r.Asks) != 1 || r.Asks[0].ID != "4" {
		t.Fatalf("discussion=%d asks=%+v", len(r.Discussion), r.Asks)
	}
}

func TestEarlierRoundsAreNotPartOfTheDiscussion(t *testing.T) {
	cfg := Config{Approvers: []string{"jdoe"}, Quorum: QuorumAll, Version: 2, UploadedAt: at(30)}
	r := Evaluate(cfg, []Comment{cm("1", "dev1", 5, "old chatter about v1"), cm("2", "dev1", 40, "fresh comment on v2")})
	if len(r.Discussion) != 1 || r.Discussion[0].ID != "2" {
		t.Fatalf("%+v", r.Discussion)
	}
}

func TestSplitRound(t *testing.T) {
	owners := []string{"qa1"}
	r := SplitRound(owners, at(10), []Comment{
		cm("0", "qa1", 5, "/fix (before the cutoff, so history)"),
		cm("1", "dev1", 11, "/fix I say so"),                // a developer is not authorized
		cm("2", "qa1", 12, "/fix the export drops rows"),    // authorized
		cm("3", "qa1", 13, "/qa"),                           // authorized
		cm("4", "dev1", 14, "/ask which build was tested?"), // anyone may ask
		cm("5", "pm1", 15, "fine by me"),
	})
	if len(r.Commands["fix"]) != 1 || r.Commands["fix"][0].ID != "2" || len(r.Commands["qa"]) != 1 {
		t.Fatalf("commands = %+v", r.Commands)
	}
	if len(r.Discussion) != 3 || len(r.Asks) != 1 || r.Asks[0].ID != "4" {
		t.Fatalf("discussion=%d asks=%+v", len(r.Discussion), r.Asks)
	}
}
