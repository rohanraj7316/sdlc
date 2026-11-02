package agents

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
)

// Demo is an offline, deterministic adapter that plays a three-turn PRD interview and
// echoes the PRD template back. It lets you exercise the whole `grill` flow with no model,
// no network and no CLI installed. Area names must match interview.TaxonomyAreas.
type Demo struct{}

func NewDemo() *Demo { return &Demo{} }

func (d *Demo) Name() string { return "demo" }
func (d *Demo) Capabilities() Capabilities {
	return Capabilities{ReadOnlySandbox: true}
}

var answeredRe = regexp.MustCompile(`(?m)^A\d+: `)

type demoUpdate struct {
	Area   string `json:"area"`
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

func (d *Demo) Run(_ context.Context, req RunRequest) (RunResult, error) {
	if strings.Contains(req.Prompt, "## Question from") { // an /ask on a document
		return RunResult{FinalText: "(demo answer) The document does not settle this. The approvers should decide; one of them can request it with /changes."}, nil
	}
	if len(req.OutputSchema) == 0 { // PRD-writing call: return the template section as-is
		return RunResult{FinalText: demoPRD(req.Prompt)}, nil
	}
	turn := len(answeredRe.FindAllString(req.Prompt, -1))
	reply := map[string]any{"done": false, "security_notes": []string{}}
	switch turn {
	case 0:
		reply["question"] = "Who is the primary user, and what concrete problem do they hit today?"
		reply["why_it_matters"] = "Everything else (scope, requirements) hangs off the user and the problem."
		reply["recommended_answer"] = "Operations analysts who reconcile reports by hand every month."
		reply["ledger_updates"] = []demoUpdate{{"Problem & evidence", "answered", ""}, {"Users", "answered", ""}}
	case 1:
		reply["question"] = "What is explicitly in scope for this release, and what is out?"
		reply["why_it_matters"] = "A tight scope prevents the build from drifting."
		reply["recommended_answer"] = "In: automated matching and an exceptions list. Out: editing source reports."
		reply["ledger_updates"] = []demoUpdate{{"Scope", "answered", ""}, {"Functional requirements", "answered", ""}}
	case 2:
		reply["question"] = "How will we know it works? Give one observable acceptance example."
		reply["why_it_matters"] = "Acceptance criteria become the QA test basis."
		reply["recommended_answer"] = "Given a matched pair, when the analyst opens the run, then no manual review item is raised."
		reply["ledger_updates"] = []demoUpdate{{"Acceptance criteria", "answered", ""}}
	default:
		reply["done"] = true
		reply["question"] = ""
		reply["ledger_updates"] = []demoUpdate{
			{"Business rules & edge cases", "assumed", "demo assumption"},
			{"Success metrics", "assumed", "demo assumption"},
			{"Non-functional expectations", "assumed", "demo assumption"},
			{"Risks", "assumed", "demo assumption"},
			{"Dependencies & stakeholders", "out_of_scope", "demo"},
			{"Rollout & communication", "out_of_scope", "demo"},
		}
	}
	b, _ := json.Marshal(reply)
	return RunResult{FinalText: string(b)}, nil
}

func demoPRD(prompt string) string {
	const start, end = "## Template (keep every heading)\n", "\n\n## Ticket seed"
	if i := strings.Index(prompt, start); i >= 0 {
		rest := prompt[i+len(start):]
		if j := strings.Index(rest, end); j >= 0 {
			rest = rest[:j]
		}
		return strings.TrimSpace(rest)
	}
	return "# PRD — demo"
}
