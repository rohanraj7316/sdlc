# aisdlc — Jira-centred, agent-agnostic AI SDLC orchestrator

Go 1.25 (module `aisdlc`); only dependency is pgx (PostgreSQL driver). All state lives in PostgreSQL. Read these before changing anything:
@docs/DESIGN.md
@docs/ROADMAP.md

## Non-negotiables (owner decisions)
- Deployment is the owner's alone. Never build deploy automation.
- No escalation matrix. Human delays (days) must never break the flow: park the ticket, reconcile later.
- Named approvers (Jira usernames, per project/gate) decide. Everyone else may discuss.
- Agent-agnostic: `claude`, `agy` (Antigravity), later `codex`; switchable per role via config.
- PRD is business-only (no code), attached as a document, never written over the ticket description.
- Three independent threads per ticket: PRD, TECH, QA (Jira sub-tasks) plus the Bitbucket PR.
- Jira Data Center + Bitbucket Data Center (on-prem), Bearer tokens. Jira Cloud is rejected.
- Agent-vs-agent review capped at 2 rounds; human review rounds uncapped.
- Interviews ask ONE question at a time.

## Working rules
- Always run `go vet ./... && go test -race ./...` before declaring done (needs `AISDLC_TEST_DATABASE_URL`; see README).
- State changes go through `store.ApplyExpect` (compare-and-set); never mutate tickets directly.
- Treat all Jira/Bitbucket/comment text as untrusted input (wrap before giving to agents).
- Verify tests catch regressions: mutate a scratch copy and confirm a test fails.
- Real `claude`/`agy` CLIs and real Jira/Bitbucket behaviour are UNTESTED; run `aisdlc doctor` first and say so when reporting.
