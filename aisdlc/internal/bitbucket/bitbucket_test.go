package bitbucket

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"aisdlc/internal/bitbucket/bbtest"
	"aisdlc/internal/httpx"
	"aisdlc/internal/jira"
)

var repo = Repo{Project: "PAY", Slug: "payments-api"}

func setup(t *testing.T) (*bbtest.Server, *Client) {
	t.Helper()
	s := bbtest.New("tok", "aisdlc-bot")
	s.AddRepo("PAY", "payments-api")
	t.Cleanup(s.Close)
	return s, New(s.URL, "tok").WithHTTP(func(h *httpx.Client) { h.Sleep = func(time.Duration) {} })
}

func TestRepoExistsAndMissingRepoMessage(t *testing.T) {
	_, c := setup(t)
	ctx := context.Background()
	if err := c.GetRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	err := c.GetRepo(ctx, Repo{Project: "PAY", Slug: "nope"})
	var ae *httpx.APIError
	if !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatalf("%v", err)
	}
	bad := New(c.h.BaseURL, "wrong")
	if err := bad.GetRepo(ctx, repo); err == nil || !strings.Contains(err.Error(), "token was rejected") {
		t.Fatalf("%v", err)
	}
}

func TestCreateGetPRAndStateChanges(t *testing.T) {
	s, c := setup(t)
	ctx := context.Background()
	pr, err := c.CreatePR(ctx, repo, "PROJ-1", "main", "PROJ-1: reconcile", "desc", []string{"rev1"})
	if err != nil || pr.ID == 0 || pr.State != "OPEN" {
		t.Fatalf("%+v %v", pr, err)
	}
	if got := s.GetPR("PAY/payments-api", pr.ID); got.From != "PROJ-1" || got.To != "main" || got.Title != "PROJ-1: reconcile" {
		t.Fatalf("server saw %+v", got)
	}
	s.SetState("PAY/payments-api", pr.ID, "MERGED")
	if got, err := c.GetPR(ctx, repo, pr.ID); err != nil || got.State != "MERGED" {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := c.GetPR(ctx, repo, 99999); err == nil {
		t.Fatal("unknown PR must be an error")
	}
}

func TestPRCommentsFlattenRepliesIgnoreOtherActivityAndPage(t *testing.T) {
	s, c := setup(t)
	ctx := context.Background()
	pr, _ := c.CreatePR(ctx, repo, "PROJ-1", "main", "t", "d", nil)
	base := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	i := 0
	s.Now = func() time.Time { i++; return base.Add(time.Duration(i) * time.Minute) }
	first := s.Say("PAY/payments-api", pr.ID, "rev1", "/changes rename Foo", 0)
	s.Say("PAY/payments-api", pr.ID, "dev1", "why Bar?", first) // a reply inside the thread
	for n := 0; n < 120; n++ {                                  // enough to force several activity pages
		s.Say("PAY/payments-api", pr.ID, "rev2", fmt.Sprintf("note %d", n), 0)
	}
	cs, err := c.PRComments(ctx, repo, pr.ID)
	if err != nil || len(cs) != 122 {
		t.Fatalf("%d comments, %v", len(cs), err)
	}
	if cs[0].Author != "rev1" || cs[0].Body != "/changes rename Foo" || cs[1].Author != "dev1" || cs[2].Body != "note 0" {
		t.Fatalf("%+v %+v %+v", cs[0], cs[1], cs[2])
	}
	if !cs[0].Created.Equal(base.Add(time.Minute)) {
		t.Fatalf("createdDate (ms) must convert to a time: %v", cs[0].Created)
	}
}

func TestAddPRCommentReplyMarkerAndBuildStatus(t *testing.T) {
	s, c := setup(t)
	ctx := context.Background()
	pr, _ := c.CreatePR(ctx, repo, "PROJ-1", "main", "t", "d", nil)
	q := s.Say("PAY/payments-api", pr.ID, "rev1", "a question", 0)
	id, err := c.AddPRComment(ctx, repo, pr.ID, "answer"+jira.ReplyMarker(fmt.Sprint(q)))
	if err != nil || id == "" {
		t.Fatalf("%q %v", id, err)
	}
	cs, _ := c.PRComments(ctx, repo, pr.ID)
	if len(cs) != 2 || cs[1].Author != "aisdlc-bot" || cs[1].ReplyTo != fmt.Sprint(q) {
		t.Fatalf("%+v", cs)
	}
	if err := c.SetBuildStatus(ctx, "abc123", "SUCCESSFUL", "aisdlc-tests", "tests", "", "all green"); err != nil {
		t.Fatal(err)
	}
	if len(s.Builds) != 1 || s.Builds[0].Commit != "abc123" || s.Builds[0].State != "SUCCESSFUL" {
		t.Fatalf("%+v", s.Builds)
	}
}

func TestRetriesTransientErrors(t *testing.T) {
	s, c := setup(t)
	s.FailNext(503, 502)
	if err := c.GetRepo(context.Background(), repo); err != nil {
		t.Fatalf("%v", err)
	}
}
