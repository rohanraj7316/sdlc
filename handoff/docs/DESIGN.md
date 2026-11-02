# Design

## Flow
1. PRD interview on a Jira ticket (AI asks one question at a time until coverage is complete) -> business-only PRD, attached as a document.
2. PRD sign-off by named approvers ("okay, done", "good to go", "lgtm", "approved", "sign off").
3. Dev interview: AI has the PRD, proposes architecture + repos, developer confirms.
4. Tech-plan sign-off (same gate).
5. Build on branch named with ticket ID. Orchestrator runs tests/lint, commits locally, does not push.
6. Independent AI review by a different agent; builder answers/fixes; max 2 rounds.
7. Push + PR to Bitbucket DC with test/coverage report attached.
8. Human code review (async, uncapped rounds).
9. Merge, then AI QA on main (manual trigger): functional vs PRD acceptance criteria, regression, performance.
10. Deployment: owner only.

## States
NEW ... DONE plus ESCALATED, FAILED, CANCELLED. Parked (nothing running, waiting on people): PRD_REVIEW, TECH_REVIEW, PR_OPEN, MERGED, QA_FAILED, QA_PASSED.
PR_REWORK: EvPRChanges increments PRRound, resets ReviewRound/BuildAttempts; EvFixed -> BUILD_VERIFY.
QA_FAILED: EvQATrigger increments QARun and reruns; EvQAFixRequested -> BUILDING.

## Durability
- `aisdlc reconcile` recomputes everything from the tracker; it wakes parked tickets.
- `ApplyExpect(Expect{State, PRDVersion, TechVersion, PRRound, QARun})` returns ErrStale so "v1 approved" never applies to v2.
- Central PostgreSQL store: one row per ticket (JSONB + state column); every write is a transaction holding the row lock (`SELECT ... FOR UPDATE`). Artifacts, feedback, interview sessions and the event log are tables too. Feedback saved when it arrives (`Feedback{Changes, Discussion, Roles}`); quiet period default 10m before a revision.

## Comment commands (first line)
| Input | Who | Effect |
|---|---|---|
| approval phrase | designated approvers | counts toward gate |
| `/changes` | approvers + code reviewers | requests revision; withdraws that approver's own approval |
| `/fix`, `/qa` | QA owners | request fix / trigger QA run |
| `/ask` | anyone | AI answers once |
| anything else | anyone | discussion: context only, never a trigger |

Approval regex rejects "done, but fix X" (that is discussion). Approvals bind to a document version; comments before the upload are ignored; only created time counts (edits ignored). A plain question does not withdraw approval.

## Threads
`[PRD]`, `[TECH]`, `[QA]` Jira sub-tasks (adopted by prefix if present); Bitbucket PR is the PR thread. Bot replies carry `(aisdlc) in reply to comment <id>`; bot housekeeping notes are filtered from discussion.

## Agents
Interface: Name(), Capabilities(), Run(ctx, RunRequest) (RunResult, error).
Roles: prd_interviewer, tech_proposer, builder, reviewer, qa.
Adapters: claude (`claude -p`, stdin prompt, `--output-format json`, `--allowedTools`), agy (`agy -p ... --output-format json`, optional script(1) pty, per-role HOME settings.json, `--conversation`, transcript fallback, 100KB argv guard), llmapi (Anthropic Messages API), demo, fake.
Independence: interviewer != builder, builder != reviewer; warning by default, error if `policy.strict_independence`.

## Config (aisdlc.example.json)
data_dir (comment files, agent homes); database{url_env}; jira{base_url, flavor, token_env, trigger_label, subtask_type, jql}; bitbucket{base_url, token_env}; approvers{PROJ:{prd,tech,pr,qa:{required[],quorum all|any}}, "*"}; participants{user:role}; bot_user; agents{role:{adapter,sandbox,timeout}}; policy; limits{review_rounds:2, build_attempts:2, max_questions:40}; claude{bin}; agy{...}; llmapi{...}.

## Layout
cmd/aisdlc (grill, status, reconcile, revise, gate-check, config-check, adapters-test, doctor, intake, publish, version); cmd/fakeatlassian (sandbox Jira :8081, Bitbucket :8082, admin :8083); internal/{agents,config,gates,orchestrator,store,interview,tracker,reconcile,httpx,jira(+jiratest),bitbucket(+bbtest),atlassian}.
Jira REST v2, `X-Atlassian-Token: no-check` for uploads; Bitbucket PR activities for comments; build-status API.
