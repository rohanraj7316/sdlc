# Roadmap

## Built (last verified: all packages passed `go test -race`)
Grill/PRD generation, gates, reconciler, central PostgreSQL store, revise loop, Jira + Bitbucket clients with simulated servers, sandbox, adapters, doctor, intake.

## Unverified
Real Jira/Bitbucket instances (run `aisdlc doctor` first); real `claude`/`agy` CLIs; PRD quality with real approvers; Bitbucket build-status on the owner's version (least certain).

## Next, in order
1. `run` worker loop: leases, concurrency cap (~10 tickets/day).
2. Dev-interview runner: proposer grounded in the real repo.
3. Workspace manager: repo catalog, mirror clones, worktree per ticket, per-repo locks, rebase before push.
4. Builder runner: orchestrator runs tests/lint/commit (agent does not self-report).
5. Independent reviewer loop: different vendor, 2-round cap, findings must cite a concrete failure.
6. Push + PR runner with build status and test report attached.
7. PR_REWORK and post-QA-fix runners.
8. QA runner: AC->test matrix, regression, performance.
9. Jira-native async PRD interview.
10. Per-ticket feature memory log owned by the orchestrator (not vendor memory), stored per-role session IDs, context pack.
11. Safety: sandbox/containers, budgets, security gate, traceability, staleness detection, notifications.

## Open decisions (owner)
- Should "end to end" stop at QA sign-off, or include release prep / production feedback?
- Use yokai / OpenHands / Paperclip as execution layer, or build runners?
- Confirm Jira Data Center vs Cloud.
- Security approval to send code to cloud models?
- Antigravity ToS for automated use?
