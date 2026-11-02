// Package gates decides whether a document (PRD or technical plan) has been signed off.
// There is deliberately no LLM in this path: approval is a pure function of comments.
package gates

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Comment struct {
	ID      string    `json:"id"`
	Author  string    `json:"author"` // accountId (Cloud) or username (Data Center)
	Created time.Time `json:"created"`
	Body    string    `json:"body"`
	ReplyTo string    `json:"reply_to,omitempty"` // set on the assistant's answer to an /ask
}

type Quorum string

const (
	QuorumAll Quorum = "all" // every approver must approve
	QuorumAny Quorum = "any" // one approver is enough
)

type Config struct {
	Approvers []string
	Quorum    Quorum
	// Version is the current artifact version (e.g. 3 for PRD_v3).
	Version int
	// UploadedAt is when that version was attached. Only comments created strictly after
	// it can approve it, so any revision resets all earlier sign-offs.
	UploadedAt time.Time
}

type Approval struct {
	Approver  string
	CommentID string
	At        time.Time
}

type Result struct {
	Passed     bool
	Approvals  []Approval
	Pending    []string  // approvers who have not (validly) approved
	Changes    []Comment // /changes requests from designated approvers (these trigger a revision)
	Discussion []Comment // every other comment after the upload, from anyone (context, never a trigger)
	Asks       []Comment // /ask comments from anyone (also in Discussion): wait for an AI answer
	Stale      []Comment // approvals ignored: wrong/older version or before the upload
}

// Approval phrases. Matched against the FIRST non-empty line only, and the line must end
// right after the phrase (plus an optional "from my side" and version), so "done, but fix X"
// or "not approved" is feedback, never an approval.
var approveRe = regexp.MustCompile(`(?i)^\s*(?:ok(?:ay)?[,.!]?\s+)?(?:/approve|approved?|lgtm|good to go|done|sign(?:ed)?[- ]?off)` +
	`(?:\s+(?:from|on)\s+my\s+side)?\s*(?:v(\d+))?\s*[.!]*\s*$`)

func firstLine(body string) string {
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) != "" {
			return l
		}
	}
	return ""
}

// ParseApproval reports whether body is an approval and, if it names a version, which one
// (0 = none named).
func ParseApproval(body string) (ok bool, version int) {
	m := approveRe.FindStringSubmatch(firstLine(body))
	if m == nil {
		return false, 0
	}
	if m[1] != "" {
		version, _ = strconv.Atoi(m[1])
	}
	return true, version
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}

// Commands a person can type as the first line of a comment. Only designated people can
// make /changes, /fix and /qa do anything; /ask works for everyone. Anything else is
// discussion: it is kept as context but never moves a ticket, so a question is never mistaken
// for a request to rewrite the document.
var commandRe = regexp.MustCompile(`(?i)^\s*/(changes|ask|fix|qa)\b`)

// ParseCommand returns "changes", "ask", "fix", "qa", or "" if the comment has no command.
func ParseCommand(body string) string {
	m := commandRe.FindStringSubmatch(firstLine(body))
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

// Evaluate recomputes the gate from scratch from the full comment list. Call it on every
// poll: deleted approvals and newer artifact versions automatically reopen the gate.
// Comment edits are ignored by construction because only Created time is considered.
//
// Only designated approvers can approve or request changes. Their /changes withdraws their own
// earlier approval; a plain question or remark does not. Everyone's other comments after the
// upload are returned as Discussion so a revision can see the whole conversation.
func Evaluate(cfg Config, comments []Comment) Result {
	cs := append([]Comment(nil), comments...)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Created.Before(cs[j].Created) })

	var res Result
	approved := map[string]Approval{}
	for _, c := range cs {
		isApprover := contains(cfg.Approvers, c.Author)
		if !c.Created.After(cfg.UploadedAt) {
			if isApprover {
				if ok, _ := ParseApproval(c.Body); ok {
					res.Stale = append(res.Stale, c)
				}
			}
			continue // earlier rounds are not part of this version's conversation
		}
		ok, v := ParseApproval(c.Body)
		cmd := ParseCommand(c.Body)
		switch {
		case isApprover && ok:
			if v != 0 && v != cfg.Version {
				res.Stale = append(res.Stale, c)
			} else {
				approved[strings.ToLower(c.Author)] = Approval{Approver: c.Author, CommentID: c.ID, At: c.Created}
			}
		case isApprover && cmd == "changes":
			res.Changes = append(res.Changes, c)
			delete(approved, strings.ToLower(c.Author))
		default:
			res.Discussion = append(res.Discussion, c)
			if cmd == "ask" {
				res.Asks = append(res.Asks, c)
			}
		}
	}

	for _, a := range cfg.Approvers {
		if ap, ok := approved[strings.ToLower(a)]; ok {
			res.Approvals = append(res.Approvals, ap)
		} else {
			res.Pending = append(res.Pending, a)
		}
	}
	switch cfg.Quorum {
	case QuorumAny:
		res.Passed = len(res.Approvals) >= 1
	default:
		res.Passed = len(cfg.Approvers) > 0 && len(res.Pending) == 0
	}
	return res
}

// Round is one stretch of a conversation (after a PR push, or after a QA report) split into
// the commands designated people issued and everything else that was said.
type Round struct {
	Commands   map[string][]Comment // command -> comments by authorized people (oldest first)
	Discussion []Comment            // all other comments after `since`, from anyone
	Asks       []Comment            // /ask comments from anyone (also in Discussion)
}

// SplitRound is Evaluate's counterpart for threads that have no document to approve (PR review,
// QA). Only authorized people's /changes, /fix and /qa count as commands.
func SplitRound(authorized []string, since time.Time, comments []Comment) Round {
	cs := append([]Comment(nil), comments...)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Created.Before(cs[j].Created) })
	r := Round{Commands: map[string][]Comment{}}
	for _, c := range cs {
		if !c.Created.After(since) {
			continue
		}
		cmd := ParseCommand(c.Body)
		switch {
		case cmd != "" && cmd != "ask" && contains(authorized, c.Author):
			r.Commands[cmd] = append(r.Commands[cmd], c)
		default:
			r.Discussion = append(r.Discussion, c)
			if cmd == "ask" {
				r.Asks = append(r.Asks, c)
			}
		}
	}
	return r
}
