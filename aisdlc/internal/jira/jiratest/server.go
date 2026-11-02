// Package jiratest is a small in-memory Jira Data Center look-alike (real HTTP, real JSON) for
// tests. It follows the documented REST v2 shapes; it proves the client handles those shapes,
// not that a given real Jira behaves identically (that is what `aisdlc doctor` is for).
package jiratest

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Comment struct{ ID, Author, Body, Created string }
type Issue struct {
	Key, Summary, Description, Status, Project, Parent string
	Labels                                             []string
	Comments                                           []Comment
	Attachments                                        []string // filenames
	AttachmentData                                     map[string][]byte
}

type Server struct {
	*httptest.Server
	Token string
	Me    string // the token's user name
	Now   func() time.Time

	mu       sync.Mutex
	issues   map[string]*Issue
	nextID   int
	seq      map[string]int // per-project counters for new keys
	failNext []int          // status codes to fail the next requests with
	Requests []string       // "METHOD path" log
}

func New(token, me string) *Server { return NewOn("127.0.0.1:0", token, me) }

// NewOn listens on a fixed address (for the local sandbox), e.g. "127.0.0.1:8081".
func NewOn(addr, token, me string) *Server {
	s := &Server{Token: token, Me: me, Now: time.Now, issues: map[string]*Issue{}, seq: map[string]int{}, nextID: 1000}
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

func (s *Server) AddIssue(key, summary, desc string, labels ...string) *Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	is := &Issue{Key: key, Summary: summary, Description: desc, Status: "To Do", Project: key[:strings.LastIndex(key, "-")], Labels: labels, AttachmentData: map[string][]byte{}}
	s.issues[key] = is
	return is
}

// Say adds a comment as a human.
func (s *Server) Say(key, author, body string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addComment(key, author, body)
}

func (s *Server) addComment(key, author, body string) string {
	s.nextID++
	id := strconv.Itoa(s.nextID)
	is := s.issues[key]
	is.Comments = append(is.Comments, Comment{ID: id, Author: author, Body: body, Created: s.Now().Format("2006-01-02T15:04:05.000-0700")})
	return id
}

func (s *Server) Get(key string) *Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	if is, ok := s.issues[key]; ok {
		cp := *is
		cp.Comments = append([]Comment(nil), is.Comments...)
		cp.Attachments = append([]string(nil), is.Attachments...)
		return &cp
	}
	return nil
}

func (s *Server) SubtasksOf(parent string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k, is := range s.issues {
		if is.Parent == parent {
			out = append(out, k)
		}
	}
	return out
}

// FailNext makes the next len(codes) API requests fail with those statuses.
func (s *Server) FailNext(codes ...int) { s.mu.Lock(); s.failNext = codes; s.mu.Unlock() }

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = append(s.Requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer "+s.Token {
		http.Error(w, `{"errorMessages":["bad token"]}`, 401)
		return
	}
	if len(s.failNext) > 0 {
		code := s.failNext[0]
		s.failNext = s.failNext[1:]
		http.Error(w, "boom", code)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/rest/api/2/")
	parts := strings.Split(path, "/")
	js := func(v any) { w.Header().Set("Content-Type", "application/json"); json.NewEncoder(w).Encode(v) }
	switch {
	case path == "myself":
		js(map[string]string{"name": s.Me, "displayName": "Bot " + s.Me})
	case path == "serverInfo":
		js(map[string]string{"version": "9.12.0", "deploymentType": "Server"})
	case path == "issuetype":
		js([]map[string]any{{"name": "Task", "subtask": false}, {"name": "Sub-task", "subtask": true}})
	case path == "search":
		jql := r.URL.Query().Get("jql")
		var keys []string
		for k, is := range s.issues {
			if strings.Contains(jql, `labels = "`) {
				lab := strings.Split(strings.Split(jql, `labels = "`)[1], `"`)[0]
				has := false
				for _, l := range is.Labels {
					has = has || l == lab
				}
				if !has {
					continue
				}
			}
			if is.Parent == "" {
				keys = append(keys, k)
			}
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
		max, _ := strconv.Atoi(r.URL.Query().Get("maxResults"))
		sortStrings(keys)
		total := len(keys)
		if start > len(keys) {
			start = len(keys)
		}
		keys = keys[start:]
		if max > 0 && len(keys) > max {
			keys = keys[:max]
		}
		var issues []map[string]string
		for _, k := range keys {
			issues = append(issues, map[string]string{"key": k})
		}
		js(map[string]any{"total": total, "issues": issues})
	case path == "issue" && r.Method == "POST": // create sub-task
		var body struct {
			Fields struct {
				Project   struct{ Key string }
				Parent    struct{ Key string }
				Summary   string
				IssueType struct{ Name string } `json:"issuetype"`
			}
		}
		json.NewDecoder(r.Body).Decode(&body)
		if _, ok := s.issues[body.Fields.Parent.Key]; !ok {
			http.Error(w, `{"errorMessages":["parent not found"]}`, 400)
			return
		}
		s.seq[body.Fields.Project.Key]++
		key := fmt.Sprintf("%s-%d", body.Fields.Project.Key, 900+s.seq[body.Fields.Project.Key])
		s.issues[key] = &Issue{Key: key, Summary: body.Fields.Summary, Project: body.Fields.Project.Key, Parent: body.Fields.Parent.Key, Status: "To Do", AttachmentData: map[string][]byte{}}
		w.WriteHeader(201)
		js(map[string]string{"key": key})
	case len(parts) >= 2 && parts[0] == "issue":
		key := parts[1]
		is, ok := s.issues[key]
		if !ok {
			http.Error(w, `{"errorMessages":["Issue does not exist or you do not have permission to see it."]}`, 404)
			return
		}
		switch {
		case len(parts) == 2 && r.Method == "GET":
			var subs []map[string]any
			for k, c := range s.issues {
				if c.Parent == key {
					subs = append(subs, map[string]any{"key": k, "fields": map[string]string{"summary": c.Summary}})
				}
			}
			js(map[string]any{"key": key, "fields": map[string]any{
				"summary": is.Summary, "description": is.Description, "labels": is.Labels,
				"status": map[string]string{"name": is.Status}, "project": map[string]string{"key": is.Project}, "subtasks": subs}})
		case len(parts) == 3 && parts[2] == "comment" && r.Method == "GET":
			start, _ := strconv.Atoi(r.URL.Query().Get("startAt"))
			max, _ := strconv.Atoi(r.URL.Query().Get("maxResults"))
			total := len(is.Comments)
			cs := is.Comments
			if start > len(cs) {
				start = len(cs)
			}
			cs = cs[start:]
			if max > 0 && len(cs) > max {
				cs = cs[:max]
			}
			var out []map[string]any
			for _, c := range cs {
				out = append(out, map[string]any{"id": c.ID, "body": c.Body, "created": c.Created, "author": map[string]string{"name": c.Author, "displayName": c.Author}})
			}
			js(map[string]any{"startAt": start, "total": total, "comments": out})
		case len(parts) == 3 && parts[2] == "comment" && r.Method == "POST":
			var b struct{ Body string }
			json.NewDecoder(r.Body).Decode(&b)
			id := s.addComment(key, s.Me, b.Body)
			w.WriteHeader(201)
			js(map[string]string{"id": id})
		case len(parts) == 3 && parts[2] == "attachments" && r.Method == "POST":
			if r.Header.Get("X-Atlassian-Token") != "no-check" {
				http.Error(w, "XSRF check failed", 403)
				return
			}
			_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				if p.FormName() == "file" {
					data, _ := io.ReadAll(p)
					is.Attachments = append(is.Attachments, p.FileName())
					is.AttachmentData[p.FileName()] = data
				}
			}
			js([]map[string]string{{"filename": "ok"}})
		default:
			http.NotFound(w, r)
		}
	default:
		http.NotFound(w, r)
	}
}

func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
