// Package bbtest is a small in-memory Bitbucket Data Center look-alike for tests (real HTTP,
// documented REST shapes). It proves the client handles those shapes, not that a given real
// Bitbucket behaves identically; `aisdlc doctor` is for that.
package bbtest

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Comment struct {
	ID      int
	Author  string
	Text    string
	Created time.Time
	Parent  int // 0 = top level
}

type PR struct {
	ID       int
	State    string
	Title    string
	From, To string
	Desc     string
	Comments []Comment
}

type BuildStatus struct{ Commit, State, Key string }

type Server struct {
	*httptest.Server
	Token string
	Me    string
	Now   func() time.Time

	mu       sync.Mutex
	Repos    map[string]bool // "PROJ/slug"
	PRs      map[string]map[int]*PR
	Builds   []BuildStatus
	nextID   int
	failNext []int
}

func New(token, me string) *Server { return NewOn("127.0.0.1:0", token, me) }

// NewOn listens on a fixed address (for the local sandbox), e.g. "127.0.0.1:8082".
func NewOn(addr, token, me string) *Server {
	s := &Server{Token: token, Me: me, Now: time.Now, Repos: map[string]bool{}, PRs: map[string]map[int]*PR{}, nextID: 100}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(s.handle))
	l, err := net.Listen("tcp", addr)
	if err != nil {
		panic(err)
	}
	ts.Listener.Close()
	ts.Listener = l
	ts.Start()
	s.Server = ts
	return s
}

func (s *Server) AddRepo(project, slug string) {
	s.mu.Lock()
	s.Repos[project+"/"+slug] = true
	s.mu.Unlock()
}

func (s *Server) FailNext(codes ...int) { s.mu.Lock(); s.failNext = codes; s.mu.Unlock() }

// Say adds a human comment (parent 0 for top level).
func (s *Server) Say(repo string, pr int, author, text string, parent int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	p := s.PRs[repo][pr]
	p.Comments = append(p.Comments, Comment{ID: s.nextID, Author: author, Text: text, Created: s.Now(), Parent: parent})
	return s.nextID
}

func (s *Server) SetState(repo string, pr int, state string) {
	s.mu.Lock()
	s.PRs[repo][pr].State = state
	s.mu.Unlock()
}

func (s *Server) GetPR(repo string, pr int) PR {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *s.PRs[repo][pr]
	cp.Comments = append([]Comment(nil), cp.Comments...)
	return cp
}

var (
	prRe    = regexp.MustCompile(`^/rest/api/latest/projects/([^/]+)/repos/([^/]+)/pull-requests(?:/(\d+)(?:/(activities|comments))?)?$`)
	repoRe  = regexp.MustCompile(`^/rest/api/latest/projects/([^/]+)/repos/([^/]+)$`)
	buildRe = regexp.MustCompile(`^/rest/build-status/1.0/commits/([^/]+)$`)
)

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		http.Error(w, `{"errors":[{"message":"Authentication failed"}]}`, 401)
		return
	}
	if len(s.failNext) > 0 {
		code := s.failNext[0]
		s.failNext = s.failNext[1:]
		http.Error(w, "boom", code)
		return
	}
	js := func(v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
	path := r.URL.Path
	switch {
	case repoRe.MatchString(path):
		m := repoRe.FindStringSubmatch(path)
		if !s.Repos[m[1]+"/"+m[2]] {
			http.Error(w, `{"errors":[{"message":"Repository does not exist"}]}`, 404)
			return
		}
		js(map[string]string{"slug": m[2]})
	case buildRe.MatchString(path) && r.Method == "POST":
		var b struct{ State, Key string }
		json.NewDecoder(r.Body).Decode(&b)
		s.Builds = append(s.Builds, BuildStatus{Commit: buildRe.FindStringSubmatch(path)[1], State: b.State, Key: b.Key})
		w.WriteHeader(204)
	case prRe.MatchString(path):
		m := prRe.FindStringSubmatch(path)
		repo := m[1] + "/" + m[2]
		if !s.Repos[repo] {
			http.Error(w, `{"errors":[{"message":"Repository does not exist"}]}`, 404)
			return
		}
		if m[3] == "" && r.Method == "POST" {
			var b struct {
				Title, Description string
				FromRef, ToRef     struct{ ID string }
			}
			json.NewDecoder(r.Body).Decode(&b)
			s.nextID++
			id := s.nextID
			if s.PRs[repo] == nil {
				s.PRs[repo] = map[int]*PR{}
			}
			s.PRs[repo][id] = &PR{ID: id, State: "OPEN", Title: b.Title, Desc: b.Description,
				From: strings.TrimPrefix(b.FromRef.ID, "refs/heads/"), To: strings.TrimPrefix(b.ToRef.ID, "refs/heads/")}
			w.WriteHeader(201)
			js(map[string]any{"id": id, "version": 0, "state": "OPEN", "title": b.Title})
			return
		}
		id, _ := strconv.Atoi(m[3])
		pr := s.PRs[repo][id]
		if pr == nil {
			http.Error(w, `{"errors":[{"message":"Pull request does not exist"}]}`, 404)
			return
		}
		switch {
		case m[4] == "" && r.Method == "GET":
			js(map[string]any{"id": pr.ID, "version": 0, "state": pr.State, "title": pr.Title})
		case m[4] == "comments" && r.Method == "POST":
			var b struct{ Text string }
			json.NewDecoder(r.Body).Decode(&b)
			s.nextID++
			pr.Comments = append(pr.Comments, Comment{ID: s.nextID, Author: s.Me, Text: b.Text, Created: s.Now()})
			w.WriteHeader(201)
			js(map[string]any{"id": s.nextID, "text": b.Text})
		case m[4] == "activities" && r.Method == "GET":
			start, _ := strconv.Atoi(r.URL.Query().Get("start"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if limit <= 0 {
				limit = 25
			}
			// Activities: one per top-level comment (replies nested), plus noise a real PR has.
			var acts []map[string]any
			acts = append(acts, map[string]any{"action": "OPENED"})
			for _, c := range pr.Comments {
				if c.Parent == 0 {
					acts = append(acts, map[string]any{"action": "COMMENTED", "comment": s.commentJSON(pr, c)})
					acts = append(acts, map[string]any{"action": "RESCOPED"})
				}
			}
			end := start + limit
			if end > len(acts) {
				end = len(acts)
			}
			if start > len(acts) {
				start = len(acts)
			}
			js(map[string]any{"values": acts[start:end], "isLastPage": end >= len(acts), "nextPageStart": end})
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) commentJSON(pr *PR, c Comment) map[string]any {
	var replies []map[string]any
	for _, r := range pr.Comments {
		if r.Parent == c.ID {
			replies = append(replies, s.commentJSON(pr, r))
		}
	}
	return map[string]any{"id": c.ID, "text": c.Text, "createdDate": c.Created.UnixMilli(),
		"author": map[string]string{"name": c.Author, "slug": c.Author}, "comments": replies}
}

var _ = fmt.Sprintf
