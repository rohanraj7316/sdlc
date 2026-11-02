// Package bitbucket is a client for the parts of the Bitbucket Data Center REST API the
// pipeline needs: open a pull request, read its state, read and post its comments, and report
// build status. Pushing code is plain git (see the workspace package), not this API.
package bitbucket

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"time"

	"aisdlc/internal/gates"
	"aisdlc/internal/httpx"
)

type Client struct{ h *httpx.Client }

func New(baseURL, token string) *Client {
	return &Client{h: &httpx.Client{BaseURL: baseURL, Token: token, Name: "Bitbucket"}}
}

func (c *Client) WithHTTP(f func(*httpx.Client)) *Client { f(c.h); return c }

type Repo struct{ Project, Slug string }

func (r Repo) path() string {
	return "/rest/api/latest/projects/" + url.PathEscape(r.Project) + "/repos/" + url.PathEscape(r.Slug)
}

type PR struct {
	ID      int
	Version int
	State   string // OPEN | MERGED | DECLINED
	Title   string
}

// GetRepo checks the repository exists and the token can read it.
func (c *Client) GetRepo(ctx context.Context, r Repo) error {
	return c.h.DoJSON(ctx, "GET", r.path(), nil, nil)
}

type refSpec struct {
	ID         string `json:"id"`
	Repository struct {
		Slug    string `json:"slug"`
		Project struct {
			Key string `json:"key"`
		} `json:"project"`
	} `json:"repository"`
}

func ref(r Repo, branch string) refSpec {
	var s refSpec
	s.ID = "refs/heads/" + branch
	s.Repository.Slug = r.Slug
	s.Repository.Project.Key = r.Project
	return s
}

type prJSON struct {
	ID      int    `json:"id"`
	Version int    `json:"version"`
	State   string `json:"state"`
	Title   string `json:"title"`
}

func (p prJSON) pr() PR { return PR{ID: p.ID, Version: p.Version, State: p.State, Title: p.Title} }

// CreatePR opens a pull request from `from` into `to` within one repository.
func (c *Client) CreatePR(ctx context.Context, r Repo, from, to, title, description string, reviewers []string) (PR, error) {
	var rv []map[string]any
	for _, u := range reviewers {
		rv = append(rv, map[string]any{"user": map[string]string{"name": u}})
	}
	body := map[string]any{"title": title, "description": description, "fromRef": ref(r, from), "toRef": ref(r, to), "reviewers": rv}
	var out prJSON
	if err := c.h.DoJSON(ctx, "POST", r.path()+"/pull-requests", body, &out); err != nil {
		return PR{}, err
	}
	return out.pr(), nil
}

func (c *Client) GetPR(ctx context.Context, r Repo, id int) (PR, error) {
	var out prJSON
	if err := c.h.DoJSON(ctx, "GET", r.path()+"/pull-requests/"+strconv.Itoa(id), nil, &out); err != nil {
		return PR{}, err
	}
	return out.pr(), nil
}

type commentJSON struct {
	ID          int    `json:"id"`
	Text        string `json:"text"`
	CreatedDate int64  `json:"createdDate"` // milliseconds since the epoch
	Author      struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	} `json:"author"`
	Comments []commentJSON `json:"comments"` // replies
}

var replyRe = regexp.MustCompile(`(?m)^\(aisdlc\) in reply to comment (\S+)\s*$`)

func flatten(c commentJSON, out *[]gates.Comment) {
	g := gates.Comment{ID: strconv.Itoa(c.ID), Author: firstNonEmpty(c.Author.Name, c.Author.Slug), Created: time.UnixMilli(c.CreatedDate).UTC(), Body: c.Text}
	if m := replyRe.FindStringSubmatch(c.Text); m != nil {
		g.ReplyTo = m[1]
	}
	*out = append(*out, g)
	for _, r := range c.Comments {
		flatten(r, out)
	}
}

// PRComments returns every comment (including replies) on a pull request, oldest first. Only
// COMMENTED activities are read; approvals, pushes and merges are not comments.
func (c *Client) PRComments(ctx context.Context, r Repo, id int) ([]gates.Comment, error) {
	var out []gates.Comment
	for start := 0; ; {
		var page struct {
			Values []struct {
				Action  string      `json:"action"`
				Comment commentJSON `json:"comment"`
			} `json:"values"`
			IsLastPage    bool `json:"isLastPage"`
			NextPageStart int  `json:"nextPageStart"`
		}
		path := fmt.Sprintf("%s/pull-requests/%d/activities?limit=100&start=%d", r.path(), id, start)
		if err := c.h.DoJSON(ctx, "GET", path, nil, &page); err != nil {
			return nil, err
		}
		for _, v := range page.Values {
			if v.Action == "COMMENTED" {
				flatten(v.Comment, &out)
			}
		}
		if page.IsLastPage || len(page.Values) == 0 {
			break
		}
		start = page.NextPageStart
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

func (c *Client) AddPRComment(ctx context.Context, r Repo, id int, text string) (string, error) {
	var out commentJSON
	err := c.h.DoJSON(ctx, "POST", fmt.Sprintf("%s/pull-requests/%d/comments", r.path(), id), map[string]string{"text": text}, &out)
	return strconv.Itoa(out.ID), err
}

// SetBuildStatus reports SUCCESSFUL / FAILED / INPROGRESS for a commit, so the PR shows the
// orchestrator's own test result. Best effort: callers should not fail a ticket over it.
func (c *Client) SetBuildStatus(ctx context.Context, commit, state, key, name, link, description string) error {
	return c.h.DoJSON(ctx, "POST", "/rest/build-status/1.0/commits/"+url.PathEscape(commit),
		map[string]string{"state": state, "key": key, "name": name, "url": link, "description": description}, nil)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
