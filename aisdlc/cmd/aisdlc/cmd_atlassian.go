package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"aisdlc/internal/atlassian"
	"aisdlc/internal/bitbucket"
	"aisdlc/internal/config"
	"aisdlc/internal/jira"
	"aisdlc/internal/store"
	"aisdlc/internal/tracker"
)

// newHub connects to Jira (and Bitbucket if configured). It returns nil, nil when Jira is not
// configured, so every command still works from files alone.
func newHub(cfg config.Config, st *store.Store) (*atlassian.Hub, error) {
	if !cfg.JiraEnabled() {
		return nil, nil
	}
	if err := cfg.CheckIntegrations(); err != nil {
		return nil, err
	}
	jt := os.Getenv(cfg.Jira.TokenEnv)
	if jt == "" {
		return nil, fmt.Errorf("environment variable %s is not set; it must hold a Jira personal access token", cfg.Jira.TokenEnv)
	}
	h := &atlassian.Hub{Jira: jira.New(cfg.Jira.BaseURL, jt), Store: st, SubtaskType: cfg.Jira.SubtaskType}
	if strings.TrimSpace(cfg.Bitbucket.BaseURL) != "" {
		bt := os.Getenv(cfg.Bitbucket.TokenEnv)
		if bt == "" {
			return nil, fmt.Errorf("environment variable %s is not set; it must hold a Bitbucket HTTP access token", cfg.Bitbucket.TokenEnv)
		}
		h.BB = bitbucket.New(cfg.Bitbucket.BaseURL, bt)
	}
	return h, nil
}

// botName makes the assistant's own comments recognisable: they are authored by whoever owns
// the Jira token, so that is who the reconciler must treat as "the AI assistant".
func botName(ctx context.Context, cfg config.Config, h *atlassian.Hub) string {
	if h == nil {
		return cfg.BotUser
	}
	if me, err := h.Jira.Myself(ctx); err == nil && me.Name != "" {
		if !strings.EqualFold(me.Name, cfg.BotUser) {
			fmt.Fprintf(os.Stderr, "note: the Jira token belongs to %q, so that is treated as the AI assistant (bot_user in the config says %q)\n", me.Name, cfg.BotUser)
		}
		return me.Name
	}
	return cfg.BotUser
}

// publishDoc attaches a stored artifact to its Jira thread and flags it published.
func publishDoc(ctx context.Context, h *atlassian.Hub, st *store.Store, key, kind string, a store.Artifact, replaces int, changelog string) error {
	th := tracker.ThreadPRD
	if kind == "tech" {
		th = tracker.ThreadTech
	}
	content, err := st.ArtifactContent(key, kind, a.Version)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%s_%s_v%d.md", key, strings.ToUpper(kind), a.Version)
	if err := h.Publish(ctx, key, th, name, content, atlassian.DocNote(kind, a.Version, replaces, changelog)); err != nil {
		return err
	}
	return st.MarkPublished(key, kind, a.Version)
}

// ---- publish ------------------------------------------------------------------------------

func cmdPublish(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return exitUsage, usageError{"usage: aisdlc publish KEY [--config F] [--data DIR]"}
	}
	key, err := normalizeKey(args[0])
	if err != nil {
		return exitUsage, err
	}
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to aisdlc.json")
	if err := fs.Parse(args[1:]); err != nil {
		return exitUsage, usageError{err.Error()}
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return exitErr, err
	}
	st, err := openStore(cfg)
	if err != nil {
		return exitErr, err
	}
	hub, err := newHub(cfg, st)
	if err != nil {
		return exitErr, err
	}
	if hub == nil {
		return exitErr, errors.New("Jira is not configured (set jira.base_url in the config)")
	}
	tk, err := st.Load(key)
	if err != nil {
		return exitErr, err
	}
	n := 0
	for _, kind := range []string{"prd", "tech"} {
		a, ok := tk.LatestArtifact(kind)
		if !ok || a.Published {
			continue
		}
		replaces := 0
		if a.Version > 1 {
			replaces = a.Version - 1
		}
		if err := publishDoc(ctx, hub, st, key, kind, a, replaces, ""); err != nil {
			return exitErr, err
		}
		fmt.Printf("published %s v%d to the %s thread\n", kind, a.Version, kind)
		n++
	}
	if n == 0 {
		fmt.Println("nothing to publish: the latest documents are already attached")
	}
	return exitOK, nil
}

// ---- intake -------------------------------------------------------------------------------

func cmdIntake(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("intake", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to aisdlc.json")
	once := fs.Bool("once", false, "run a single pass and exit")
	every := fs.Duration("every", 2*time.Minute, "pause between passes when running continuously")
	if err := fs.Parse(args); err != nil {
		return exitUsage, usageError{err.Error()}
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return exitErr, err
	}
	st, err := openStore(cfg)
	if err != nil {
		return exitErr, err
	}
	hub, err := newHub(cfg, st)
	if err != nil {
		return exitErr, err
	}
	if hub == nil {
		return exitErr, errors.New("Jira is not configured (set jira.base_url in the config)")
	}
	pass := func() {
		keys, err := hub.Intake(ctx, cfg.Jira.TriggerLabel, cfg.Jira.JQL)
		for _, k := range keys {
			fmt.Printf("%s  picked up %s; start its PRD interview with: aisdlc grill %s\n", time.Now().Format("01-02 15:04:05"), k, k)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "intake:", err)
		}
	}
	if *once {
		pass()
		return exitOK, nil
	}
	fmt.Printf("Looking for tickets labelled %q every %s. Ctrl-C to stop; it is safe to restart.\n", cfg.Jira.TriggerLabel, *every)
	for {
		pass()
		select {
		case <-ctx.Done():
			return exitOK, nil
		case <-time.After(*every):
		}
	}
}

// ---- doctor -------------------------------------------------------------------------------

// cmdDoctor is the first thing to run against your real systems. Everything it does is read-only.
func cmdDoctor(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to aisdlc.json")
	issue := fs.String("issue", "", "also read this Jira issue (e.g. PROJ-123) and its comments")
	repoFlag := fs.String("repo", "", "also check read access to this Bitbucket repository (PROJECT/slug)")
	if err := fs.Parse(args); err != nil {
		return exitUsage, usageError{err.Error()}
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return exitErr, err
	}
	fails := 0
	pass := func(f string, a ...any) { fmt.Printf("  PASS  %s\n", fmt.Sprintf(f, a...)) }
	warn := func(f string, a ...any) { fmt.Printf("  WARN  %s\n", fmt.Sprintf(f, a...)) }
	fail := func(f string, a ...any) { fails++; fmt.Printf("  FAIL  %s\n", fmt.Sprintf(f, a...)) }

	fmt.Println("Jira")
	if !cfg.JiraEnabled() {
		warn("jira.base_url is not set: running from files only")
	} else if err := cfg.CheckIntegrations(); err != nil {
		fail("%v", err)
	} else {
		jt := os.Getenv(cfg.Jira.TokenEnv)
		if jt == "" {
			fail("environment variable %s is not set (a Jira personal access token)", cfg.Jira.TokenEnv)
		} else {
			j := jira.New(cfg.Jira.BaseURL, jt)
			if si, err := j.ServerInfo(ctx); err != nil {
				fail("serverInfo: %v", err)
			} else {
				pass("reachable: version %s, deploymentType %q", si.Version, si.DeploymentType)
				if strings.EqualFold(si.DeploymentType, "Cloud") {
					warn("this is Jira Cloud, which this build does not support (it targets Data Center)")
				}
			}
			if me, err := j.Myself(ctx); err != nil {
				fail("who am I: %v", err)
			} else {
				pass("token belongs to %q (%s); the assistant's comments will carry this name", me.Name, me.DisplayName)
				if !strings.EqualFold(me.Name, cfg.BotUser) {
					warn("bot_user is %q but the token's user is %q: the reconciler uses %q, no change needed unless you want them to match", cfg.BotUser, me.Name, me.Name)
				}
			}
			if types, err := j.IssueTypes(ctx); err != nil {
				fail("issue types: %v", err)
			} else {
				found := false
				for _, t := range types {
					if t.Name == cfg.Jira.SubtaskType {
						found = true
						if !t.Subtask {
							fail("issue type %q exists but is not a sub-task type", t.Name)
						} else {
							pass("sub-task issue type %q exists", t.Name)
						}
					}
				}
				if !found {
					fail("no issue type named %q (set jira.subtask_type to your sub-task type's exact name)", cfg.Jira.SubtaskType)
				}
			}
			jql := fmt.Sprintf(`labels = "%s" AND statusCategory != Done AND issuetype not in subTaskIssueTypes()`, cfg.Jira.TriggerLabel)
			if keys, err := j.Search(ctx, jql, 50); err != nil {
				fail("search %q: %v", jql, err)
			} else {
				pass("search works: %d open ticket(s) carry the label %q", len(keys), cfg.Jira.TriggerLabel)
			}
			if *issue != "" {
				key, kerr := normalizeKey(*issue)
				if kerr != nil {
					return exitUsage, kerr
				}
				if is, err := j.GetIssue(ctx, key); err != nil {
					fail("read %s: %v", key, err)
				} else {
					pass("read %s: %q (status %s, %d sub-task(s))", key, is.Summary, is.Status, len(is.Subtasks))
					if cs, err := j.Comments(ctx, key); err != nil {
						fail("comments on %s: %v", key, err)
					} else {
						pass("read %d comment(s) on %s (timestamps parsed)", len(cs), key)
					}
				}
			}
		}
	}

	fmt.Println("Bitbucket")
	if strings.TrimSpace(cfg.Bitbucket.BaseURL) == "" {
		warn("bitbucket.base_url is not set: the pull-request thread will stay empty")
	} else {
		bt := os.Getenv(cfg.Bitbucket.TokenEnv)
		if bt == "" {
			fail("environment variable %s is not set (a Bitbucket HTTP access token)", cfg.Bitbucket.TokenEnv)
		} else if *repoFlag == "" {
			warn("pass --repo PROJECT/slug to check read access to a repository")
		} else {
			parts := strings.SplitN(*repoFlag, "/", 2)
			if len(parts) != 2 {
				return exitUsage, usageError{"--repo must look like PROJECT/slug"}
			}
			if err := bitbucket.New(cfg.Bitbucket.BaseURL, bt).GetRepo(ctx, bitbucket.Repo{Project: parts[0], Slug: parts[1]}); err != nil {
				fail("repository %s: %v", *repoFlag, err)
			} else {
				pass("can read repository %s", *repoFlag)
			}
		}
	}
	fmt.Println()
	if fails > 0 {
		fmt.Printf("%d check(s) failed. Fix those first; paste the FAIL lines if you want help.\n", fails)
		return exitNotReady, nil
	}
	fmt.Println("All checks passed. This only proves reading works; posting and attaching are exercised by `aisdlc intake` on a test ticket.")
	return exitOK, nil
}
