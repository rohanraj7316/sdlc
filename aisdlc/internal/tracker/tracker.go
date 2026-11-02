// Package tracker is where conversations live. The reconciler needs three capabilities:
//
//	Tracker   read every comment on a ticket's thread (required)
//	Poster    write a comment back, e.g. an AI answer or a changelog (optional)
//	PRWatcher ask whether the pull request is still open, merged or declined (optional)
//
// A Jira (and Bitbucket) client will implement the same interfaces; DirTracker lets the whole
// flow run today from files.
package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aisdlc/internal/gates"
)

// Thread is one conversation on a ticket. They stay separate on purpose, so the product
// discussion, the technical discussion, code review and QA never drown each other out.
type Thread string

const (
	ThreadPRD  Thread = "prd"
	ThreadTech Thread = "tech"
	ThreadPR   Thread = "pr" // human code review on the pull request
	ThreadQA   Thread = "qa"
)

var knownThreads = map[Thread]bool{ThreadPRD: true, ThreadTech: true, ThreadPR: true, ThreadQA: true}

type Tracker interface {
	// Comments returns all comments on the thread. A thread nobody has commented on yet is
	// an empty list, not an error. Errors are treated as transient by callers.
	Comments(ctx context.Context, key string, th Thread) ([]gates.Comment, error)
}

// Poster is implemented by trackers the assistant may write to. Callers must treat it as
// optional: a read-only tracker simply means no AI replies.
type Poster interface {
	Post(ctx context.Context, key string, th Thread, c gates.Comment) error
}

type PRState string

const (
	PROpen     PRState = "open"
	PRMerged   PRState = "merged"
	PRDeclined PRState = "declined"
)

type PRWatcher interface {
	PRState(ctx context.Context, key string) (PRState, error)
}

// Func adapts a function to Tracker (handy in tests).
type Func func(ctx context.Context, key string, th Thread) ([]gates.Comment, error)

func (f Func) Comments(ctx context.Context, key string, th Thread) ([]gates.Comment, error) {
	return f(ctx, key, th)
}

// DirTracker reads <Dir>/<KEY>/<thread>.json: [{"id","author","created","body","reply_to"}, ...].
// Anyone (or any script, or a future Jira poller) can drop or append comments there. The PR's
// state is the single word open|merged|declined in <Dir>/<KEY>/pr_state (missing means open).
type DirTracker struct{ Dir string }

func (d DirTracker) path(key string, th Thread) (string, error) {
	if !knownThreads[th] {
		return "", fmt.Errorf("unknown thread %q", th)
	}
	return filepath.Join(d.Dir, key, string(th)+".json"), nil
}

func (d DirTracker) Comments(_ context.Context, key string, th Thread) ([]gates.Comment, error) {
	p, err := d.path(key, th)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cs []gates.Comment
	if err := json.Unmarshal(b, &cs); err != nil {
		return nil, fmt.Errorf("%s/%s.json: %w", key, th, err)
	}
	return cs, nil
}

// Post appends a comment atomically (temp file + rename).
func (d DirTracker) Post(ctx context.Context, key string, th Thread, c gates.Comment) error {
	p, err := d.path(key, th)
	if err != nil {
		return err
	}
	cs, err := d.Comments(ctx, key, th)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(append(cs, c), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (d DirTracker) PRState(_ context.Context, key string) (PRState, error) {
	b, err := os.ReadFile(filepath.Join(d.Dir, key, "pr_state"))
	if errors.Is(err, os.ErrNotExist) {
		return PROpen, nil
	}
	if err != nil {
		return "", err
	}
	switch s := PRState(strings.ToLower(strings.TrimSpace(string(b)))); s {
	case PROpen, PRMerged, PRDeclined:
		return s, nil
	default:
		return "", fmt.Errorf("%s/pr_state: unknown state %q (want open|merged|declined)", key, s)
	}
}
