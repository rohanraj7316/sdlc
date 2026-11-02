// Package atlassian joins the Jira and Bitbucket clients into the four conversation threads the
// reconciler understands:
//
//	prd, tech, qa  comments on three sub-tasks of the ticket ([PRD], [TECH], [QA]) in Jira, so the
//	               three conversations stay independent and each can be attached to and discussed
//	pr             comments on the ticket's pull request in Bitbucket
//
// Hub implements tracker.Tracker, tracker.Poster and tracker.PRWatcher, so swapping the
// file-based tracker for the real systems changes nothing else.
package atlassian

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"aisdlc/internal/bitbucket"
	"aisdlc/internal/gates"
	"aisdlc/internal/jira"
	"aisdlc/internal/store"
	"aisdlc/internal/tracker"
)

type Hub struct {
	Jira        *jira.Client
	BB          *bitbucket.Client // nil: no code host, so the pr thread is always empty
	Store       *store.Store
	SubtaskType string // issue type name for sub-tasks (default "Sub-task")
}

var (
	_ tracker.Tracker   = (*Hub)(nil)
	_ tracker.Poster    = (*Hub)(nil)
	_ tracker.PRWatcher = (*Hub)(nil)
)

var prefixes = map[tracker.Thread]string{tracker.ThreadPRD: "[PRD]", tracker.ThreadTech: "[TECH]", tracker.ThreadQA: "[QA]"}

// Threads that live on Jira sub-tasks, in creation order.
var jiraThreads = []tracker.Thread{tracker.ThreadPRD, tracker.ThreadTech, tracker.ThreadQA}

func (h *Hub) subtaskType() string {
	if h.SubtaskType == "" {
		return "Sub-task"
	}
	return h.SubtaskType
}

// EnsureThreads makes sure the [PRD], [TECH] and [QA] sub-tasks exist for a ticket (adopting
// ones that already exist, creating the rest) and records them. It returns which it created.
func (h *Hub) EnsureThreads(ctx context.Context, key string) (created []tracker.Thread, err error) {
	for _, th := range jiraThreads {
		_, made, err := h.subtask(ctx, key, th)
		if err != nil {
			return created, err
		}
		if made {
			created = append(created, th)
		}
	}
	return created, nil
}

func (h *Hub) subtask(ctx context.Context, key string, th tracker.Thread) (subKey string, created bool, err error) {
	prefix, ok := prefixes[th]
	if !ok {
		return "", false, fmt.Errorf("thread %q does not live on a Jira sub-task", th)
	}
	tk, err := h.Store.Load(key)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", false, err
	}
	if tk != nil && tk.Subtasks[string(th)] != "" {
		return tk.Subtasks[string(th)], false, nil
	}
	parent, err := h.Jira.GetIssue(ctx, key)
	if err != nil {
		return "", false, err
	}
	for _, st := range parent.Subtasks { // adopt one a person (or an earlier run) already made
		if strings.HasPrefix(st.Summary, prefix) {
			got, err := h.Store.SetSubtask(key, string(th), st.Key)
			return got, false, err
		}
	}
	summary := prefix + " " + parent.Summary
	if len(summary) > 250 {
		summary = summary[:250]
	}
	newKey, err := h.Jira.CreateSubtask(ctx, key, summary, h.subtaskType())
	if err != nil {
		return "", false, err
	}
	got, err := h.Store.SetSubtask(key, string(th), newKey)
	return got, got == newKey, err
}

func (h *Hub) pr(key string) (bitbucket.Repo, int, bool, error) {
	tk, err := h.Store.Load(key)
	if errors.Is(err, store.ErrNotFound) {
		return bitbucket.Repo{}, 0, false, nil
	}
	if err != nil {
		return bitbucket.Repo{}, 0, false, err
	}
	if tk.PR == nil || h.BB == nil {
		return bitbucket.Repo{}, 0, false, nil
	}
	return bitbucket.Repo{Project: tk.PR.Project, Slug: tk.PR.Repo}, tk.PR.ID, true, nil
}

func (h *Hub) Comments(ctx context.Context, key string, th tracker.Thread) ([]gates.Comment, error) {
	if th == tracker.ThreadPR {
		repo, id, ok, err := h.pr(key)
		if err != nil || !ok {
			return nil, err // no pull request yet: an empty thread
		}
		return h.BB.PRComments(ctx, repo, id)
	}
	sub, _, err := h.subtask(ctx, key, th)
	if err != nil {
		return nil, err
	}
	return h.Jira.Comments(ctx, sub)
}

func (h *Hub) Post(ctx context.Context, key string, th tracker.Thread, c gates.Comment) error {
	body := c.Body
	if c.ReplyTo != "" {
		body += jira.ReplyMarker(c.ReplyTo)
	}
	if th == tracker.ThreadPR {
		repo, id, ok, err := h.pr(key)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s has no pull request recorded, so there is nowhere to post", key)
		}
		_, err = h.BB.AddPRComment(ctx, repo, id, body)
		return err
	}
	sub, _, err := h.subtask(ctx, key, th)
	if err != nil {
		return err
	}
	_, err = h.Jira.AddComment(ctx, sub, body)
	return err
}

func (h *Hub) PRState(ctx context.Context, key string) (tracker.PRState, error) {
	repo, id, ok, err := h.pr(key)
	if err != nil || !ok {
		return tracker.PROpen, err
	}
	pr, err := h.BB.GetPR(ctx, repo, id)
	if err != nil {
		return "", err
	}
	switch strings.ToUpper(pr.State) {
	case "MERGED":
		return tracker.PRMerged, nil
	case "DECLINED":
		return tracker.PRDeclined, nil
	}
	return tracker.PROpen, nil
}

// Publish attaches a document to a thread's sub-task and posts a note beside it.
func (h *Hub) Publish(ctx context.Context, key string, th tracker.Thread, filename string, content []byte, note string) error {
	sub, _, err := h.subtask(ctx, key, th)
	if err != nil {
		return err
	}
	if err := h.Jira.Attach(ctx, sub, filename, content); err != nil {
		return fmt.Errorf("attach %s to %s: %w", filename, sub, err)
	}
	if _, err := h.Jira.AddComment(ctx, sub, note); err != nil {
		return fmt.Errorf("post note on %s: %w", sub, err)
	}
	return nil
}

// Seed returns the ticket's title and the text to seed the PRD interview with.
func (h *Hub) Seed(ctx context.Context, key string) (title, text string, err error) {
	is, err := h.Jira.GetIssue(ctx, key)
	if err != nil {
		return "", "", err
	}
	return is.Summary, strings.TrimSpace(is.Summary + "\n\n" + is.Description), nil
}

// WelcomeNote explains the conversation rules to everyone on the thread, once.
func WelcomeNote(key string) string {
	return "aisdlc picked up " + key + ". A PRD interview will produce a PRD and attach it to this sub-task.\n\n" +
		"How to take part here:\n" +
		"* Talk freely. Questions, opinions and suggestions are kept as context and never change the document by themselves.\n" +
		"* /ask <question>: anyone can ask, and the AI assistant answers here from the document.\n" +
		"* /changes <what to change>: designated approvers use this to send the document back for a new version.\n" +
		"* approved (or: okay, done / good to go): designated approvers sign off the current version.\n" +
		"Only the designated approvers decide; everyone else is welcome to discuss."
}

// Intake finds tickets carrying the trigger label that the pipeline does not know yet, registers
// them, and makes sure their threads exist. It is idempotent and safe to run on a timer.
func (h *Hub) Intake(ctx context.Context, label, extraJQL string) (newKeys []string, err error) {
	jql := fmt.Sprintf(`labels = "%s" AND statusCategory != Done AND issuetype not in subTaskIssueTypes()`, label)
	if strings.TrimSpace(extraJQL) != "" {
		jql += " AND (" + extraJQL + ")"
	}
	keys, err := h.Jira.Search(ctx, jql, 500)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, key := range keys {
		if verr := store.ValidKey(key); verr != nil {
			errs = append(errs, verr)
			continue
		}
		created, rerr := h.Store.Register(key)
		if rerr != nil {
			errs = append(errs, rerr)
			continue
		}
		made, terr := h.EnsureThreads(ctx, key)
		if terr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, terr))
			continue
		}
		for _, th := range made {
			if th == tracker.ThreadPRD {
				if perr := h.Post(ctx, key, tracker.ThreadPRD, gates.Comment{Body: WelcomeNote(key)}); perr != nil {
					errs = append(errs, fmt.Errorf("%s: welcome note: %w", key, perr))
				}
			}
		}
		if created {
			newKeys = append(newKeys, key)
		}
	}
	return newKeys, errors.Join(errs...)
}

// DocNote is the comment posted beside a newly attached document. It repeats the rules so a
// reviewer who opens the sub-task for the first time knows what to type.
func DocNote(kind string, version, replaces int, changelog string) string {
	label := map[string]string{"prd": "PRD", "tech": "Technical plan"}[kind]
	if label == "" {
		label = strings.ToUpper(kind)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s v%d is attached and ready for review.", label, version)
	if replaces > 0 {
		fmt.Fprintf(&b, " It replaces v%d; earlier approvals do not carry over.", replaces)
	}
	if strings.TrimSpace(changelog) != "" {
		b.WriteString("\n\nChangelog:\n" + strings.TrimSpace(changelog))
	}
	fmt.Fprintf(&b, "\n\nDesignated approvers: reply \"approved\" to sign off v%d, or \"/changes <what to change>\" to send it back. "+
		"Anyone: \"/ask <question>\" for an answer, or just discuss.", version)
	return b.String()
}
