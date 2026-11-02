// Package jira is a client for the parts of the Jira Data Center REST API (v2) the pipeline
// needs: read an issue, find tickets by label, read and post comments, attach documents, and
// create the [PRD]/[TECH]/[QA] sub-tasks that carry the three independent threads.
//
// It targets Data Center (personal access token, plain-text/wiki comment bodies). Jira Cloud
// needs the v3 API and Atlassian Document Format and is not supported yet.
package jira

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"aisdlc/internal/gates"
	"aisdlc/internal/httpx"
)

type Client struct{ h *httpx.Client }

// New returns a client. The token is a Jira personal access token.
func New(baseURL, token string) *Client {
	return &Client{h: &httpx.Client{BaseURL: baseURL, Token: token, Name: "Jira"}}
}

// WithHTTP exposes the shared transport for tests (retry sleeps, custom http.Client).
func (c *Client) WithHTTP(f func(*httpx.Client)) *Client { f(c.h); return c }

type User struct{ Name, DisplayName string }

type ServerInfo struct {
	Version        string `json:"version"`
	DeploymentType string `json:"deploymentType"`
}

type SubtaskRef struct{ Key, Summary string }

type Issue struct {
	Key, Summary, Description, Status, Project string
	Labels                                     []string
	Subtasks                                   []SubtaskRef
}

type IssueType struct {
	Name    string `json:"name"`
	Subtask bool   `json:"subtask"`
}

func (c *Client) Myself(ctx context.Context) (User, error) {
	var r struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	}
	if err := c.h.DoJSON(ctx, "GET", "/rest/api/2/myself", nil, &r); err != nil {
		return User{}, err
	}
	return User{Name: r.Name, DisplayName: r.DisplayName}, nil
}

func (c *Client) ServerInfo(ctx context.Context) (ServerInfo, error) {
	var r ServerInfo
	return r, c.h.DoJSON(ctx, "GET", "/rest/api/2/serverInfo", nil, &r)
}

func (c *Client) IssueTypes(ctx context.Context) ([]IssueType, error) {
	var r []IssueType
	return r, c.h.DoJSON(ctx, "GET", "/rest/api/2/issuetype", nil, &r)
}

func (c *Client) GetIssue(ctx context.Context, key string) (Issue, error) {
	var r struct {
		Key    string `json:"key"`
		Fields struct {
			Summary     string   `json:"summary"`
			Description string   `json:"description"`
			Labels      []string `json:"labels"`
			Status      struct {
				Name string `json:"name"`
			} `json:"status"`
			Project struct {
				Key string `json:"key"`
			} `json:"project"`
			Subtasks []struct {
				Key    string `json:"key"`
				Fields struct {
					Summary string `json:"summary"`
				} `json:"fields"`
			} `json:"subtasks"`
		} `json:"fields"`
	}
	path := "/rest/api/2/issue/" + url.PathEscape(key) + "?fields=summary,description,labels,status,project,subtasks"
	if err := c.h.DoJSON(ctx, "GET", path, nil, &r); err != nil {
		return Issue{}, err
	}
	is := Issue{Key: r.Key, Summary: r.Fields.Summary, Description: r.Fields.Description, Status: r.Fields.Status.Name,
		Project: r.Fields.Project.Key, Labels: r.Fields.Labels}
	for _, st := range r.Fields.Subtasks {
		is.Subtasks = append(is.Subtasks, SubtaskRef{Key: st.Key, Summary: st.Fields.Summary})
	}
	return is, nil
}

// Search returns the keys of issues matching jql (all pages, up to max).
func (c *Client) Search(ctx context.Context, jql string, max int) ([]string, error) {
	if max <= 0 {
		max = 200
	}
	var keys []string
	for start := 0; len(keys) < max; {
		var r struct {
			Total  int `json:"total"`
			Issues []struct {
				Key string `json:"key"`
			} `json:"issues"`
		}
		q := url.Values{"jql": {jql}, "fields": {"key"}, "startAt": {strconv.Itoa(start)}, "maxResults": {"100"}}
		if err := c.h.DoJSON(ctx, "GET", "/rest/api/2/search?"+q.Encode(), nil, &r); err != nil {
			return nil, err
		}
		for _, is := range r.Issues {
			keys = append(keys, is.Key)
		}
		start += len(r.Issues)
		if len(r.Issues) == 0 || start >= r.Total {
			break
		}
	}
	if len(keys) > max {
		keys = keys[:max]
	}
	return keys, nil
}

var replyRe = regexp.MustCompile(`(?m)^\(aisdlc\) in reply to comment (\S+)\s*$`)

// ReplyMarker is appended to the assistant's answers so "which question did this answer" survives
// in the thread itself (Jira has no reply-to field); Comments parses it back into ReplyTo.
func ReplyMarker(commentID string) string {
	return "\n\n(aisdlc) in reply to comment " + commentID
}

// Comments returns every comment on an issue, oldest first, across all pages. Authors are the
// Jira username (the same string you list under "approvers").
func (c *Client) Comments(ctx context.Context, key string) ([]gates.Comment, error) {
	var out []gates.Comment
	for start := 0; ; {
		var r struct {
			Total    int `json:"total"`
			Comments []struct {
				ID     string `json:"id"`
				Body   string `json:"body"`
				Create string `json:"created"`
				Author struct {
					Name        string `json:"name"`
					AccountID   string `json:"accountId"`
					DisplayName string `json:"displayName"`
				} `json:"author"`
			} `json:"comments"`
		}
		path := fmt.Sprintf("/rest/api/2/issue/%s/comment?orderBy=created&startAt=%d&maxResults=100", url.PathEscape(key), start)
		if err := c.h.DoJSON(ctx, "GET", path, nil, &r); err != nil {
			return nil, err
		}
		for _, cm := range r.Comments {
			t, err := ParseTime(cm.Create)
			if err != nil {
				return nil, fmt.Errorf("comment %s on %s: %w", cm.ID, key, err)
			}
			author := firstNonEmpty(cm.Author.Name, cm.Author.AccountID, cm.Author.DisplayName)
			g := gates.Comment{ID: cm.ID, Author: author, Created: t, Body: cm.Body}
			if m := replyRe.FindStringSubmatch(cm.Body); m != nil {
				g.ReplyTo = m[1]
			}
			out = append(out, g)
		}
		start += len(r.Comments)
		if len(r.Comments) == 0 || start >= r.Total {
			return out, nil
		}
	}
}

func (c *Client) AddComment(ctx context.Context, key, body string) (string, error) {
	var r struct {
		ID string `json:"id"`
	}
	err := c.h.DoJSON(ctx, "POST", "/rest/api/2/issue/"+url.PathEscape(key)+"/comment", map[string]string{"body": body}, &r)
	return r.ID, err
}

// Attach uploads a file. Jira rejects uploads without the no-check XSRF header.
func (c *Client) Attach(ctx context.Context, key, filename string, data []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.h.Do(ctx, "POST", "/rest/api/2/issue/"+url.PathEscape(key)+"/attachments", buf.Bytes(), w.FormDataContentType(),
		map[string]string{"X-Atlassian-Token": "no-check"}, nil)
}

// CreateSubtask creates a sub-task of parent and returns its key.
func (c *Client) CreateSubtask(ctx context.Context, parentKey, summary, issueType string) (string, error) {
	project := parentKey
	if i := strings.LastIndex(parentKey, "-"); i > 0 {
		project = parentKey[:i]
	}
	body := map[string]any{"fields": map[string]any{
		"project":   map[string]string{"key": project},
		"parent":    map[string]string{"key": parentKey},
		"summary":   summary,
		"issuetype": map[string]string{"name": issueType},
	}}
	var r struct {
		Key string `json:"key"`
	}
	if err := c.h.DoJSON(ctx, "POST", "/rest/api/2/issue", body, &r); err != nil {
		return "", err
	}
	return r.Key, nil
}

// ParseTime reads Jira's timestamp format ("2026-10-04T10:15:30.123+0530") and RFC 3339.
func ParseTime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700", time.RFC3339Nano} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised Jira timestamp %q", s)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
