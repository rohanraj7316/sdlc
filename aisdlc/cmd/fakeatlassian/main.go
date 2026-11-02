// Command fakeatlassian is a local sandbox: simulated Jira and Bitbucket Data Center servers
// plus an admin endpoint for playing the humans, so you can try the whole pipeline with no real
// systems. It is a look-alike built from the documented API shapes, not the real thing.
//
//	go run ./cmd/fakeatlassian
//	export JIRA_TOKEN=jira-sandbox-token BITBUCKET_TOKEN=bb-sandbox-token
//	(point jira.base_url / bitbucket.base_url at the printed URLs)
//
// Admin (all GET, for curl):
//
//	/say?issue=PROJ-1&thread=prd|tech|qa&author=jdoe&body=approved
//	/prsay?repo=PAY/payments-api&pr=101&author=rev1&body=/changes+rename+Foo
//	/prstate?repo=PAY/payments-api&pr=101&state=MERGED
//	/show?issue=PROJ-1&thread=prd         (prints the thread)
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"aisdlc/internal/bitbucket/bbtest"
	"aisdlc/internal/jira/jiratest"
)

func main() {
	jp := flag.String("jira", "127.0.0.1:8081", "Jira listen address")
	bp := flag.String("bitbucket", "127.0.0.1:8082", "Bitbucket listen address")
	ap := flag.String("admin", "127.0.0.1:8083", "admin listen address")
	jt := flag.String("jira-token", "jira-sandbox-token", "Jira token")
	bt := flag.String("bitbucket-token", "bb-sandbox-token", "Bitbucket token")
	flag.Parse()

	jr := jiratest.NewOn(*jp, *jt, "aisdlc-bot")
	bb := bbtest.NewOn(*bp, *bt, "aisdlc-bot")
	bb.AddRepo("PAY", "payments-api")
	jr.AddIssue("PROJ-1", "Auto reconcile", "Operations analysts reconcile monthly reports by hand and it takes two days.", "ai-sdlc")
	jr.AddIssue("PROJ-2", "Unrelated ticket", "No label, so intake ignores it.")

	subOf := func(issue, thread string) string {
		for _, k := range jr.SubtasksOf(issue) {
			if strings.HasPrefix(strings.ToLower(jr.Get(k).Summary), "["+thread+"]") {
				return k
			}
		}
		return ""
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/say", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		sub := subOf(q.Get("issue"), q.Get("thread"))
		if sub == "" {
			http.Error(w, "no such thread yet (run `aisdlc intake` first)", 404)
			return
		}
		fmt.Fprintf(w, "comment %s added to %s\n", jr.Say(sub, q.Get("author"), q.Get("body")), sub)
	})
	mux.HandleFunc("/show", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		sub := subOf(q.Get("issue"), q.Get("thread"))
		if sub == "" {
			http.Error(w, "no such thread", 404)
			return
		}
		is := jr.Get(sub)
		fmt.Fprintf(w, "%s  %s\nattachments: %v\n", sub, is.Summary, is.Attachments)
		for _, c := range is.Comments {
			fmt.Fprintf(w, "  [%s] %s: %s\n", c.ID, c.Author, strings.ReplaceAll(c.Body, "\n", " / "))
		}
	})
	mux.HandleFunc("/prsay", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		id, _ := strconv.Atoi(q.Get("pr"))
		fmt.Fprintf(w, "comment %d\n", bb.Say(q.Get("repo"), id, q.Get("author"), q.Get("body"), 0))
	})
	mux.HandleFunc("/prstate", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		id, _ := strconv.Atoi(q.Get("pr"))
		bb.SetState(q.Get("repo"), id, strings.ToUpper(q.Get("state")))
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var keys []string
		for repo, prs := range bb.PRs {
			for id := range prs {
				keys = append(keys, fmt.Sprintf("%s#%d", repo, id))
			}
		}
		sort.Strings(keys)
		fmt.Fprintf(w, "fakeatlassian sandbox\nPRs: %v\n", keys)
	})
	fmt.Printf("Sandbox up.\n  Jira       http://%s   token %s\n  Bitbucket  http://%s   token %s\n  admin      http://%s\n", *jp, *jt, *bp, *bt, *ap)
	log.Fatal(http.ListenAndServe(*ap, mux))
}
