package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aisdlc/internal/agents"
	"aisdlc/internal/gates"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "aisdlc.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMergesPartialRoles(t *testing.T) {
	c, err := Load(write(t, `{"agents":{"reviewer":{"adapter":"claude"}},"limits":{"review_rounds":3,"build_attempts":2,"max_questions":10}}`))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Agents[agents.RoleReviewer]
	if r.Adapter != "claude" {
		t.Fatalf("adapter not applied: %+v", r)
	}
	if r.Sandbox != agents.ReadOnly || r.Timeout.Std() != 15*time.Minute {
		t.Fatalf("partial role must keep default sandbox/timeout, got %+v", r)
	}
	if c.Agents[agents.RoleBuilder].Adapter != "claude" || c.Limits.ReviewRounds != 3 {
		t.Fatalf("other roles/limits wrong: %+v", c)
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	for name, body := range map[string]string{
		"unknown top-level": `{"agnts":{}}`,
		"unknown role":      `{"agents":{"janitor":{"adapter":"claude"}}}`,
		"unknown role key":  `{"agents":{"builder":{"adaptor":"claude"}}}`,
		"bad duration":      `{"agents":{"builder":{"timeout":45}}}`,
	} {
		if _, err := Load(write(t, body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestOverridesWin(t *testing.T) {
	c := Default()
	ov := Overrides{agents.RoleBuilder: "agy"}
	if c.AdapterName(agents.RoleBuilder, ov) != "agy" || c.AdapterName(agents.RoleBuilder, nil) != "claude" {
		t.Fatal("override precedence wrong")
	}
	if got := c.Role(agents.RoleBuilder, ov); got.Adapter != "agy" || got.Sandbox != agents.WorkspaceWrite {
		t.Fatalf("Role() = %+v", got)
	}
}

func TestIndependenceWarnings(t *testing.T) {
	c := Default() // interviewer=agy, builder=claude, reviewer=agy: the clean split
	if w := c.Independence(nil); len(w) != 0 {
		t.Fatalf("default config should be clean, got %v", w)
	}
	// interviewer on the builder's vendor -> warning
	w := c.Independence(Overrides{agents.RoleInterviewer: "claude"})
	if len(w) != 1 || !strings.Contains(w[0], "prd_interviewer") {
		t.Fatalf("warnings = %v", w)
	}
	// every role on one vendor: both pairs warn
	all := Overrides{agents.RoleBuilder: "agy"}
	if w := c.Independence(all); len(w) != 2 {
		t.Fatalf("want 2 warnings, got %v", w)
	}
	// the mirror split is clean too: builder=agy, interviewer=reviewer=claude
	mirror := Overrides{agents.RoleInterviewer: "claude", agents.RoleBuilder: "agy", agents.RoleReviewer: "claude"}
	if w := c.Independence(mirror); len(w) != 0 {
		t.Fatalf("mirror split should be clean, got %v", w)
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	reg := c.BuildRegistry()

	if _, err := c.Validate(reg, nil); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
	if _, err := c.Validate(reg, Overrides{agents.RoleQA: "nope"}); err == nil {
		t.Fatal("unknown adapter must fail")
	}
	if _, err := c.Validate(reg, Overrides{agents.RoleBuilder: "llmapi"}); err == nil {
		t.Fatal("llmapi as builder must fail")
	}
	// llmapi is fine for the interviewer
	if _, err := c.Validate(reg, Overrides{agents.RoleInterviewer: "llmapi"}); err != nil {
		t.Fatalf("llmapi interviewer should be valid: %v", err)
	}
	c.Agents[agents.RoleBuilder] = RoleConfig{Adapter: "claude", Sandbox: agents.ReadOnly}
	if _, err := c.Validate(reg, nil); err == nil {
		t.Fatal("read-only builder must fail")
	}

	// strict independence turns warnings into errors; non-strict only warns
	c = Default()
	shared := Overrides{agents.RoleInterviewer: "claude"}
	w, err := c.Validate(reg, shared)
	if err != nil || len(w) != 1 {
		t.Fatalf("non-strict: want 1 warning no error, got %v / %v", w, err)
	}
	c.Policy.StrictIndependence = true
	if _, err := c.Validate(reg, shared); err == nil {
		t.Fatal("strict mode must error on shared adapters")
	}
	if _, err := c.Validate(reg, nil); err != nil {
		t.Fatalf("strict mode must accept the clean default split: %v", err)
	}
}

func TestRequestFromConfig(t *testing.T) {
	c := Default()
	r := c.Request(agents.RoleProposer, nil)
	if r.Role != agents.RoleProposer || r.Sandbox != agents.ReadOnly || r.MaxTurns != 40 || r.Timeout != 20*time.Minute {
		t.Fatalf("request = %+v", r)
	}
}

func TestApproversFor(t *testing.T) {
	c := Default()
	c.Approvers = map[string]GateApprovers{
		"PROJ": {PRD: ApproverSet{Required: []string{"jdoe", "asmith"}}, Tech: ApproverSet{Required: []string{"dev1"}, Quorum: "any"}},
		"*":    {PRD: ApproverSet{Required: []string{"fallback"}}},
	}
	if a, q := c.ApproversFor("PROJ-12", "prd"); len(a) != 2 || q != gates.QuorumAll {
		t.Fatalf("prd: %v %v", a, q)
	}
	if a, q := c.ApproversFor("PROJ-12", "tech"); len(a) != 1 || q != gates.QuorumAny {
		t.Fatalf("tech: %v %v", a, q)
	}
	if a, _ := c.ApproversFor("OTHER-3", "prd"); len(a) != 1 || a[0] != "fallback" {
		t.Fatalf("wildcard: %v", a)
	}
	if a, _ := c.ApproversFor("OTHER-3", "tech"); len(a) != 0 {
		t.Fatalf("wildcard has no tech approvers, so nobody can approve: %v", a)
	}
	if a, _ := Default().ApproversFor("PROJ-1", "prd"); len(a) != 0 {
		t.Fatal("no config means no approvers")
	}
}

func TestLoadApproversFromJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"approvers":{"PROJ":{"prd":{"required":["jdoe"],"quorum":"any"}}}}`), 0o644)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if a, q := c.ApproversFor("PROJ-9", "prd"); len(a) != 1 || a[0] != "jdoe" || q != gates.QuorumAny {
		t.Fatalf("%v %v", a, q)
	}
}

func TestApproversForPRAndQAAndRoleOf(t *testing.T) {
	c := Default()
	c.Approvers = map[string]GateApprovers{"PROJ": {
		PR: ApproverSet{Required: []string{"rev1", "rev2"}, Quorum: "any"},
		QA: ApproverSet{Required: []string{"qa1"}},
	}}
	c.Participants = map[string]string{"Dev1": "developer", "qa1": "qa", "jdoe": "product"}
	if a, q := c.ApproversFor("PROJ-5", "pr"); len(a) != 2 || q != gates.QuorumAny {
		t.Fatalf("pr: %v %v", a, q)
	}
	if a, q := c.ApproversFor("PROJ-5", "qa"); len(a) != 1 || a[0] != "qa1" || q != gates.QuorumAll {
		t.Fatalf("qa: %v %v", a, q)
	}
	if a, _ := c.ApproversFor("PROJ-5", "nonsense"); len(a) != 0 {
		t.Fatalf("unknown gate has no approvers: %v", a)
	}
	for user, want := range map[string]string{"dev1": "developer", "QA1": "qa", "aisdlc-bot": "AI assistant", "AISDLC-BOT": "AI assistant", "stranger": "participant"} {
		if got := c.RoleOf(user); got != want {
			t.Errorf("RoleOf(%q) = %q, want %q", user, got, want)
		}
	}
	if c.BotUser != "aisdlc-bot" {
		t.Fatalf("default bot user = %q", c.BotUser)
	}
}
