package jira

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"aisdlc/internal/httpx"
	"aisdlc/internal/jira/jiratest"
)

func setup(t *testing.T) (*jiratest.Server, *Client) {
	t.Helper()
	s := jiratest.New("tok", "aisdlc-bot")
	t.Cleanup(s.Close)
	c := New(s.URL, "tok").WithHTTP(func(h *httpx.Client) { h.Sleep = func(time.Duration) {} })
	return s, c
}

func TestMyselfServerInfoAndIssueTypes(t *testing.T) {
	_, c := setup(t)
	ctx := context.Background()
	me, err := c.Myself(ctx)
	if err != nil || me.Name != "aisdlc-bot" {
		t.Fatalf("%+v %v", me, err)
	}
	if si, err := c.ServerInfo(ctx); err != nil || si.DeploymentType != "Server" || si.Version == "" {
		t.Fatalf("%+v %v", si, err)
	}
	its, err := c.IssueTypes(ctx)
	if err != nil || len(its) != 2 || !its[1].Subtask {
		t.Fatalf("%+v %v", its, err)
	}
}

func TestBadTokenGivesAHelpfulError(t *testing.T) {
	s := jiratest.New("tok", "bot")
	defer s.Close()
	_, err := New(s.URL, "wrong").Myself(context.Background())
	var ae *httpx.APIError
	if !errors.As(err, &ae) || ae.Status != 401 || !strings.Contains(err.Error(), "token was rejected") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), "wrong") && strings.Contains(err.Error(), "Bearer") {
		t.Fatal("the token must never appear in errors")
	}
}

func TestGetIssueAndMissingIssue(t *testing.T) {
	s, c := setup(t)
	s.AddIssue("PROJ-1", "Auto reconcile", "Analysts reconcile by hand.", "ai-sdlc")
	is, err := c.GetIssue(context.Background(), "PROJ-1")
	if err != nil || is.Summary != "Auto reconcile" || is.Description != "Analysts reconcile by hand." || is.Project != "PROJ" || is.Labels[0] != "ai-sdlc" {
		t.Fatalf("%+v %v", is, err)
	}
	_, err = c.GetIssue(context.Background(), "PROJ-404")
	var ae *httpx.APIError
	if !errors.As(err, &ae) || ae.Status != 404 || !strings.Contains(err.Error(), "cannot see it") {
		t.Fatalf("%v", err)
	}
}

func TestSearchByLabelPagesThroughEverything(t *testing.T) {
	s, c := setup(t)
	for i := 1; i <= 230; i++ {
		s.AddIssue(fmt.Sprintf("PROJ-%d", i), "t", "d", "ai-sdlc")
	}
	s.AddIssue("PROJ-999", "unlabelled", "d")
	keys, err := c.Search(context.Background(), `labels = "ai-sdlc"`, 500)
	if err != nil || len(keys) != 230 {
		t.Fatalf("got %d keys, err %v", len(keys), err)
	}
	if capped, _ := c.Search(context.Background(), `labels = "ai-sdlc"`, 50); len(capped) != 50 {
		t.Fatalf("max must cap the result, got %d", len(capped))
	}
}

func TestCommentsReadAllPagesAndKeepOrderAuthorsAndTimes(t *testing.T) {
	s, c := setup(t)
	s.AddIssue("PROJ-1", "x", "y")
	base := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 250; i++ {
		s.Now = func() time.Time { return base.Add(time.Duration(i) * time.Minute) }
		s.Say("PROJ-1", fmt.Sprintf("user%d", i%3), fmt.Sprintf("comment %d", i))
	}
	cs, err := c.Comments(context.Background(), "PROJ-1")
	if err != nil || len(cs) != 250 {
		t.Fatalf("%d comments, %v", len(cs), err)
	}
	if cs[0].Body != "comment 0" || cs[249].Body != "comment 249" || cs[7].Author != "user1" || !cs[1].Created.Equal(base.Add(time.Minute)) {
		t.Fatalf("%+v ... %+v", cs[0], cs[1])
	}
}

func TestParseTime(t *testing.T) {
	want := time.Date(2026, 10, 4, 4, 45, 30, 123000000, time.UTC)
	for _, in := range []string{"2026-10-04T10:15:30.123+0530", "2026-10-04T04:45:30.123Z", "2026-10-04T04:45:30.123+00:00"} {
		got, err := ParseTime(in)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q -> %v %v", in, got, err)
		}
	}
	if _, err := ParseTime("yesterday"); err == nil {
		t.Fatal("garbage must be an error, not a zero time")
	}
}

func TestAddCommentAndReplyMarkerRoundTrip(t *testing.T) {
	s, c := setup(t)
	s.AddIssue("PROJ-1", "x", "y")
	q := s.Say("PROJ-1", "qa1", "/ask what is out of scope?")
	id, err := c.AddComment(context.Background(), "PROJ-1", "Answer text"+ReplyMarker(q))
	if err != nil || id == "" {
		t.Fatalf("%q %v", id, err)
	}
	cs, _ := c.Comments(context.Background(), "PROJ-1")
	if len(cs) != 2 || cs[1].Author != "aisdlc-bot" || cs[1].ReplyTo != q || cs[0].ReplyTo != "" {
		t.Fatalf("%+v", cs)
	}
}

func TestAttachSendsTheXSRFHeaderAndTheFile(t *testing.T) {
	s, c := setup(t)
	s.AddIssue("PROJ-1", "x", "y")
	if err := c.Attach(context.Background(), "PROJ-1", "PROJ-1_PRD_v1.md", []byte("# PRD v1\nhello")); err != nil {
		t.Fatal(err)
	}
	is := s.Get("PROJ-1")
	if len(is.Attachments) != 1 || is.Attachments[0] != "PROJ-1_PRD_v1.md" || string(is.AttachmentData["PROJ-1_PRD_v1.md"]) != "# PRD v1\nhello" {
		t.Fatalf("%+v", is)
	}
}

func TestCreateSubtask(t *testing.T) {
	s, c := setup(t)
	s.AddIssue("PROJ-1", "x", "y")
	key, err := c.CreateSubtask(context.Background(), "PROJ-1", "[PRD] x", "Sub-task")
	if err != nil || !strings.HasPrefix(key, "PROJ-") {
		t.Fatalf("%q %v", key, err)
	}
	is, _ := c.GetIssue(context.Background(), "PROJ-1")
	if len(is.Subtasks) != 1 || is.Subtasks[0].Key != key || is.Subtasks[0].Summary != "[PRD] x" {
		t.Fatalf("%+v", is.Subtasks)
	}
	if _, err := c.CreateSubtask(context.Background(), "PROJ-77", "[PRD] x", "Sub-task"); err == nil {
		t.Fatal("a missing parent must be an error")
	}
}

func TestRetriesGatewayErrorsThenSucceeds(t *testing.T) {
	s, c := setup(t)
	s.FailNext(503, 429, 502)
	if _, err := c.Myself(context.Background()); err != nil {
		t.Fatalf("three transient failures must be survived: %v", err)
	}
	s.FailNext(503, 503, 503, 503, 503)
	if _, err := c.Myself(context.Background()); err == nil {
		t.Fatal("retries are bounded")
	}
	s.FailNext(400) // a client error is not retried
	n := len(s.Requests)
	if _, err := c.Myself(context.Background()); err == nil || len(s.Requests)-n != 1 {
		t.Fatalf("a 400 must fail on the first attempt (%d requests)", len(s.Requests)-n)
	}
}
