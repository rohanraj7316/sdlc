// Package agents defines the vendor-neutral adapter interface. The orchestrator only
// ever calls Adapter.Run; which vendor sits behind it is a config choice per role.
package agents

import (
	"context"
	"encoding/json"
	"time"
)

type Role string

const (
	RoleInterviewer Role = "prd_interviewer"
	RoleProposer    Role = "tech_proposer"
	RoleBuilder     Role = "builder"
	RoleReviewer    Role = "reviewer"
	RoleQA          Role = "qa"
)

var AllRoles = []Role{RoleInterviewer, RoleProposer, RoleBuilder, RoleReviewer, RoleQA}

type Sandbox string

const (
	ReadOnly       Sandbox = "read_only"
	WorkspaceWrite Sandbox = "workspace_write"
)

type RunRequest struct {
	RunID        string
	Role         Role
	Workdir      string // git worktree; agent cwd. May be empty for tool-less roles.
	SystemPrompt string
	Prompt       string // untrusted data must already be wrapped by the caller
	OutputSchema []byte // optional JSON schema (enforced natively where supported)
	Sandbox      Sandbox
	NoTools      bool     // pure-conversation call: grant no tools at all
	AllowedTools []string // extra tools beyond the sandbox default (vendor syntax)
	MaxTurns     int
	Timeout      time.Duration
	Env          map[string]string // model API keys only; never Jira/Bitbucket creds
	ResumeID     string            // continue a prior session (fix loops)
}

type RunResult struct {
	SessionID      string
	FinalText      string
	Structured     json.RawMessage
	ExitCode       int
	CostUSD        float64
	TranscriptPath string
	Notes          []string // fallbacks taken (e.g. "agy: recovered answer from transcript")
}

type Capabilities struct {
	StructuredOutput bool // native schema enforcement
	Resume           bool
	ReadOnlySandbox  bool
	CostReporting    bool
}

type Adapter interface {
	Name() string
	Capabilities() Capabilities
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}
