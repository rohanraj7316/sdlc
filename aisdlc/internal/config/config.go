// Package config loads aisdlc.json and resolves which adapter runs each role.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"aisdlc/internal/agents"
	"aisdlc/internal/gates"
)

// Duration unmarshals from strings like "45m".
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"45m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }
func (d Duration) Std() time.Duration           { return time.Duration(d) }

type RoleConfig struct {
	Adapter  string         `json:"adapter"`
	Sandbox  agents.Sandbox `json:"sandbox"`
	MaxTurns int            `json:"max_turns"`
	Timeout  Duration       `json:"timeout"`
}

type AgyConfig struct {
	Bin                     string `json:"bin"`
	PTY                     string `json:"pty"`       // "" or "script"
	HomeRoot                string `json:"home_root"` // per-role HOME root
	Provider                string `json:"provider"`  // e.g. "gemini" (API-key mode)
	SkipPermissionsForWrite bool   `json:"skip_permissions_for_write"`
}

type ClaudeConfig struct {
	Bin string `json:"bin"`
}

type LLMAPIConfig struct {
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env"`
}

type Policy struct {
	// StrictIndependence turns the independence warnings into hard errors.
	StrictIndependence bool `json:"strict_independence"`
}

type Limits struct {
	ReviewRounds  int `json:"review_rounds"`
	BuildAttempts int `json:"build_attempts"`
	MaxQuestions  int `json:"max_questions"`
}

// ApproverSet is who must sign off one gate, and whether all of them or any one suffices.
type ApproverSet struct {
	Required []string `json:"required"`
	Quorum   string   `json:"quorum"` // "all" (default) | "any"
}

// GateApprovers are the sign-off rules for one Jira project.
type GateApprovers struct {
	PRD  ApproverSet `json:"prd"`  // sign-off on the PRD
	Tech ApproverSet `json:"tech"` // sign-off on the technical plan
	PR   ApproverSet `json:"pr"`   // code reviewers whose /changes send the PR back to the builder
	QA   ApproverSet `json:"qa"`   // QA owners: /qa, /fix and the final QA sign-off
}

type Config struct {
	// Approvers maps a Jira project key (the PROJ in PROJ-123) to its sign-off rules;
	// "*" is the fallback for projects not listed.
	Approvers map[string]GateApprovers `json:"approvers"`
	// Participants labels people with a role (product, developer, qa, code reviewer...) so the
	// writer and the AI see who said what. Anyone may comment; only approvers decide.
	Participants map[string]string `json:"participants"`
	// BotUser is the name the assistant's own comments carry in the tracker.
	BotUser   string                     `json:"bot_user"`
	DataDir   string                     `json:"data_dir"`
	Agents    map[agents.Role]RoleConfig `json:"agents"`
	Policy    Policy                     `json:"policy"`
	Limits    Limits                     `json:"limits"`
	Claude    ClaudeConfig               `json:"claude"`
	Agy       AgyConfig                  `json:"agy"`
	LLMAPI    LLMAPIConfig               `json:"llmapi"`
	Jira      JiraConfig                 `json:"jira"`
	Bitbucket BitbucketConfig            `json:"bitbucket"`
	Database  DatabaseConfig             `json:"database"`
}

// DatabaseConfig points at the PostgreSQL database that holds all pipeline state. The URL
// carries a password, so the config only names the environment variable holding it.
type DatabaseConfig struct {
	URLEnv string `json:"url_env"` // e.g. AISDLC_DATABASE_URL=postgres://user:pass@host:5432/aisdlc
}

// JiraConfig connects to Jira Data Center with a personal access token. Leave base_url empty to
// run without Jira (conversations then come from files).
type JiraConfig struct {
	BaseURL      string `json:"base_url"`      // e.g. https://jira.corp.local
	Flavor       string `json:"flavor"`        // "datacenter" (default); "cloud" is not supported yet
	TokenEnv     string `json:"token_env"`     // env var holding the token (never put the token in this file)
	TriggerLabel string `json:"trigger_label"` // tickets carrying this label are picked up by `intake`
	SubtaskType  string `json:"subtask_type"`  // issue type NAME used for the [PRD]/[TECH]/[QA] sub-tasks
	JQL          string `json:"jql"`           // optional extra filter ANDed with the label
}

// BitbucketConfig connects to Bitbucket Data Center with an HTTP access token.
type BitbucketConfig struct {
	BaseURL  string `json:"base_url"`
	TokenEnv string `json:"token_env"`
}

// Overrides map a role to an adapter name for one run/ticket (empty = use config).
type Overrides map[agents.Role]string

func Default() Config {
	return Config{
		DataDir: "./data",
		Agents: map[agents.Role]RoleConfig{
			agents.RoleInterviewer: {Adapter: "agy", Sandbox: agents.ReadOnly, MaxTurns: 6, Timeout: Duration(10 * time.Minute)},
			agents.RoleProposer:    {Adapter: "agy", Sandbox: agents.ReadOnly, MaxTurns: 40, Timeout: Duration(20 * time.Minute)},
			agents.RoleBuilder:     {Adapter: "claude", Sandbox: agents.WorkspaceWrite, MaxTurns: 120, Timeout: Duration(45 * time.Minute)},
			agents.RoleReviewer:    {Adapter: "agy", Sandbox: agents.ReadOnly, MaxTurns: 40, Timeout: Duration(15 * time.Minute)},
			agents.RoleQA:          {Adapter: "agy", Sandbox: agents.WorkspaceWrite, MaxTurns: 80, Timeout: Duration(30 * time.Minute)},
		},
		BotUser:   "aisdlc-bot",
		Jira:      JiraConfig{Flavor: "datacenter", TokenEnv: "JIRA_TOKEN", TriggerLabel: "ai-sdlc", SubtaskType: "Sub-task"},
		Bitbucket: BitbucketConfig{TokenEnv: "BITBUCKET_TOKEN"},
		Database:  DatabaseConfig{URLEnv: "AISDLC_DATABASE_URL"},
		Limits:    Limits{ReviewRounds: 2, BuildAttempts: 2, MaxQuestions: 40},
		Agy:       AgyConfig{PTY: "script"},
	}
}

// Load reads a JSON config on top of Default(); an empty path returns the defaults.
// Role entries merge field-by-field over the defaults, so {"adapter":"agy"} alone is valid.
func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Default(), fmt.Errorf("%s: %w", path, err)
	}
	// The decode above replaced whole role entries; redo them as merges over defaults.
	var raw struct {
		Agents map[agents.Role]json.RawMessage `json:"agents"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return Default(), fmt.Errorf("%s: %w", path, err)
	}
	def := Default()
	for role, rm := range raw.Agents {
		base, ok := def.Agents[role]
		if !ok {
			return Default(), fmt.Errorf("%s: unknown role %q", path, role)
		}
		rd := json.NewDecoder(strings.NewReader(string(rm)))
		rd.DisallowUnknownFields()
		if err := rd.Decode(&base); err != nil {
			return Default(), fmt.Errorf("%s: role %s: %w", path, role, err)
		}
		c.Agents[role] = base
	}
	for _, r := range agents.AllRoles { // roles missing from the file keep defaults
		if _, ok := c.Agents[r]; !ok {
			c.Agents[r] = def.Agents[r]
		}
	}
	return c, nil
}

// AdapterName resolves the adapter for a role: override > config.
func (c Config) AdapterName(role agents.Role, ov Overrides) string {
	if n := ov[role]; n != "" {
		return n
	}
	return c.Agents[role].Adapter
}

// Role returns the effective role settings (adapter replaced by any override).
func (c Config) Role(role agents.Role, ov Overrides) RoleConfig {
	rc := c.Agents[role]
	rc.Adapter = c.AdapterName(role, ov)
	return rc
}

// Independence returns warnings for role pairs that share an adapter where the design wants
// different agents: interviewer vs builder, builder vs reviewer. With Policy.StrictIndependence
// the caller should treat a non-empty result as an error (see Validate).
func (c Config) Independence(ov Overrides) []string {
	var w []string
	pair := func(a, b agents.Role, why string) {
		if c.AdapterName(a, ov) == c.AdapterName(b, ov) {
			w = append(w, fmt.Sprintf("%s and %s both use %q: %s", a, b, c.AdapterName(a, ov), why))
		}
	}
	pair(agents.RoleInterviewer, agents.RoleBuilder, "the grill-up agent should differ from the build agent")
	pair(agents.RoleBuilder, agents.RoleReviewer, "same-vendor review is weaker; a fresh session is still used, but not a different model")
	return w
}

// Validate checks every role has a known adapter and a usable sandbox, and enforces strict
// independence when enabled. Returns warnings (non-fatal) and an error (fatal).
func (c Config) Validate(reg *agents.Registry, ov Overrides) (warnings []string, err error) {
	for _, r := range agents.AllRoles {
		rc, ok := c.Agents[r]
		if !ok {
			return nil, fmt.Errorf("role %s is not configured", r)
		}
		name := c.AdapterName(r, ov)
		a, gerr := reg.Get(name)
		if gerr != nil {
			return nil, fmt.Errorf("role %s: %w", r, gerr)
		}
		if rc.Sandbox == agents.ReadOnly && !a.Capabilities().ReadOnlySandbox {
			return nil, fmt.Errorf("role %s: adapter %q cannot provide a read-only sandbox", r, name)
		}
		if r == agents.RoleBuilder && rc.Sandbox != agents.WorkspaceWrite {
			return nil, fmt.Errorf("role builder must use sandbox workspace_write, got %q", rc.Sandbox)
		}
		if name == "llmapi" && r != agents.RoleInterviewer {
			return nil, fmt.Errorf("role %s: adapter llmapi has no tools and is only valid for prd_interviewer", r)
		}
	}
	warnings = c.Independence(ov)
	if c.Policy.StrictIndependence && len(warnings) > 0 {
		return warnings, fmt.Errorf("strict_independence is on: %s", strings.Join(warnings, "; "))
	}
	return warnings, nil
}

// BuildRegistry wires every adapter from config. "fake" is registered by callers that want it.
func (c Config) BuildRegistry() *agents.Registry {
	reg := agents.NewRegistry()
	reg.Register(agents.NewClaude(c.Claude.Bin))
	agy := agents.NewAgy(c.Agy.Bin)
	agy.PTY, agy.HomeRoot, agy.Provider = c.Agy.PTY, c.Agy.HomeRoot, c.Agy.Provider
	agy.SkipPermissionsForWrite = c.Agy.SkipPermissionsForWrite
	reg.Register(agy)
	reg.Register(agents.NewLLMAPI(c.LLMAPI.Model, c.LLMAPI.APIKeyEnv))
	return reg
}

// Request builds a RunRequest skeleton for a role from effective settings.
func (c Config) Request(role agents.Role, ov Overrides) agents.RunRequest {
	rc := c.Role(role, ov)
	return agents.RunRequest{Role: role, Sandbox: rc.Sandbox, MaxTurns: rc.MaxTurns, Timeout: rc.Timeout.Std()}
}

// ApproversFor returns who must approve a gate ("prd" or "tech") for a ticket key. The
// project's own entry wins; "*" is the fallback. No entry means nobody can approve.
func (c Config) ApproversFor(key, gate string) ([]string, gates.Quorum) {
	project := key
	if i := strings.LastIndex(key, "-"); i > 0 {
		project = key[:i]
	}
	ga, ok := c.Approvers[project]
	if !ok {
		ga, ok = c.Approvers["*"]
	}
	if !ok {
		return nil, gates.QuorumAll
	}
	var set ApproverSet
	switch gate {
	case "prd":
		set = ga.PRD
	case "tech":
		set = ga.Tech
	case "pr":
		set = ga.PR
	case "qa":
		set = ga.QA
	}
	q := gates.QuorumAll
	if strings.EqualFold(set.Quorum, "any") {
		q = gates.QuorumAny
	}
	return set.Required, q
}

// RoleOf labels a person for the conversation transcript ("product", "developer", ...). The
// assistant is "AI assistant"; people not listed under participants are just "participant".
func (c Config) RoleOf(user string) string {
	if c.BotUser != "" && strings.EqualFold(user, c.BotUser) {
		return "AI assistant"
	}
	for name, role := range c.Participants {
		if strings.EqualFold(name, user) {
			return role
		}
	}
	return "participant"
}

// JiraEnabled reports whether a Jira base URL is configured.
func (c Config) JiraEnabled() bool { return strings.TrimSpace(c.Jira.BaseURL) != "" }

// CheckIntegrations rejects configurations the integrations cannot honour yet.
func (c Config) CheckIntegrations() error {
	if c.JiraEnabled() && !strings.EqualFold(c.Jira.Flavor, "datacenter") {
		return fmt.Errorf("jira.flavor %q is not supported yet: only \"datacenter\" (Jira Cloud needs the v3 API and Atlassian Document Format)", c.Jira.Flavor)
	}
	return nil
}
