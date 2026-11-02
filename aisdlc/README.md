# aisdlc — Jira-centred AI SDLC pipeline (skeleton)

Go; the only dependency is the PostgreSQL driver (pgx). All pipeline state lives in one central
PostgreSQL database, so every process on every machine sees the same tickets. Every role (PRD interviewer, tech proposer, builder, reviewer,
QA) can run on **Claude Code (`claude`)** or **Antigravity (`agy`)**; the interviewer can
also use **`llmapi`** (direct API, no tools). Switch in `aisdlc.json` or per run.

## Build and test
Tests need a PostgreSQL server; each test gets its own schema, dropped afterwards.

    docker run -d --name aisdlc-pg -e POSTGRES_PASSWORD=aisdlc -e POSTGRES_DB=aisdlc \
        -p 55432:5432 postgres:16-alpine -c max_connections=300
    export AISDLC_TEST_DATABASE_URL='postgres://postgres:aisdlc@127.0.0.1:55432/aisdlc?sslmode=disable'
    go build -o aisdlc ./cmd/aisdlc
    go test -race ./...

## Database
Every command reads the PostgreSQL URL from the variable named by `database.url_env` (default
`AISDLC_DATABASE_URL`; never put the URL in the config, it carries a password). Tables are created on
first use: `tickets` (state + JSONB ticket), `artifacts` (PRD / tech plan versions), `feedback`,
`sessions` (interviews), `events` (audit log). Read a stored document with `aisdlc show KEY [prd|tech]`.

    export AISDLC_DATABASE_URL='postgres://postgres:aisdlc@127.0.0.1:55432/aisdlc?sslmode=disable'

## Try it with no model and no real systems
    printf '\n\n\n' | ./aisdlc grill PROJ-1 --text "Analysts reconcile reports by hand." --interviewer demo
    ./aisdlc status PROJ-1

## Try the Jira / Bitbucket flow locally (sandbox)
`fakeatlassian` serves simulated Jira and Bitbucket Data Center servers plus an admin endpoint for
playing the humans. It is a look-alike built from the documented API shapes, not the real thing.

    go run ./cmd/fakeatlassian &                 # Jira :8081, Bitbucket :8082, admin :8083
    export JIRA_TOKEN=jira-sandbox-token BITBUCKET_TOKEN=bb-sandbox-token
    # aisdlc.json: "jira":{"base_url":"http://127.0.0.1:8081"}, "bitbucket":{"base_url":"http://127.0.0.1:8082"}
    ./aisdlc doctor --config aisdlc.json --issue PROJ-1 --repo PAY/payments-api
    ./aisdlc intake --config aisdlc.json --once              # creates [PRD]/[TECH]/[QA] sub-tasks
    printf '\n\n\n' | ./aisdlc grill PROJ-1 --config aisdlc.json --interviewer demo   # seeds from the ticket, attaches the PRD
    curl -G localhost:8083/say --data-urlencode issue=PROJ-1 --data-urlencode thread=prd \
         --data-urlencode author=jdoe --data-urlencode "body=approved"
    ./aisdlc reconcile --config aisdlc.json --once --interviewer demo --proposer demo

## Use the real agents
    cp aisdlc.example.json aisdlc.json
    ./aisdlc config-check --config aisdlc.json       # who runs what, and is it installed?
    ./aisdlc adapters-test --config aisdlc.json      # contract-test the real CLIs first
    ./aisdlc grill PROJ-123 --seed-file ticket.txt --config aisdlc.json

Per-run switching, any role: `--interviewer claude --builder agy --reviewer claude`.
Independence rules (interviewer != builder, builder != reviewer) warn by default; set
`"strict_independence": true` to make them errors.

## Antigravity notes
`agy -p` has been reported to drop stdout when not on a TTY, so `agy.pty` defaults to
`"script"` (util-linux script(1)). Run `adapters-test` on your machine before trusting it.
For a service, prefer API-key / GCP-project auth over a personal OAuth login
(`agy.provider: "gemini"` + `GEMINI_API_KEY`), and keep `skip_permissions_for_write` off
unless the builder runs in an isolated container.

## Human delay never breaks the flow
Reviewers and product owners may take hours or weeks. The design assumes that:

- **Parked, not waiting.** A ticket waiting on people sits in `PRD_REVIEW` / `TECH_REVIEW` /
  `PRD_REVISION` with nothing running: no process, no agent session, no open worktree.
- **`aisdlc reconcile` wakes tickets.** Each pass re-derives the truth from the comment
  threads (never from memory), and only if something changed applies one transition.
  Run it once from cron or leave it running. Stop it, restart it, or run several at once.
- **Stale decisions are impossible.** Transitions are compare-and-set on state *and*
  document version, so "v1 was approved" can never be applied to v2. Every write holds the
  ticket's row lock in PostgreSQL, so reconcilers and CLI commands on any machine never lose each
  other's writes.
- **Approvals are bound to a version.** Revising a PRD resets sign-offs; approvals posted
  before the upload never count; later approval outweighs earlier questions.
- **A tracker outage changes nothing.** Errors are reported and retried next pass.
- **Feedback is captured when it arrives** (the `feedback` table, per document version) and held
  until someone runs `aisdlc revise KEY`, so nothing is lost if that is days later. Feedback
  must be quiet for `--feedback-quiet` (default 10m) so a reviewer typing several comments
  triggers one revision, not several.
- `aisdlc status` shows how long each ticket has been in its state.

## Many people, many rounds: talk freely, decide explicitly
Product, developers, QA and code reviewers can all talk in the ticket's threads as much as
they like. What *moves* a ticket is explicit, and only designated people can do it:

| You write (first line)            | Who may | Effect |
|-----------------------------------|---------|--------|
| `approved`, `okay, done`, `good to go from my side`, `lgtm` | the configured approvers of that gate | sign-off (version-bound) |
| `/changes <what to change>`       | the gate's approvers / code reviewers | send the document back for revision / the PR back to the builder |
| `/fix <what>`, `/qa`              | the QA owners | send QA findings to the developer / start or repeat a QA run |
| `/ask <question>`                 | anyone | the AI answers in the thread, once, from the document and the discussion (PRD and tech plan threads) |
| anything else                     | anyone | discussion: kept as context, never a trigger |

So a question or an opinion, even from an approver, never turns into a rewrite, and an
approver who asked a question keeps the approval they already gave. **Note:** a plain remark
such as "done, but fix X" is discussion, not a change request. Use `/changes` to request one.

- **Revisions see everything.** `aisdlc revise` gives the writer the whole conversation since the
  last version, oldest first, labelled by role (`participants` in `aisdlc.json`), with the
  approvers' `/changes` marked as the requests. All of it is wrapped as untrusted data.
  When the new version lands, the changelog is posted back to the thread.
- **The AI can reply.** With a tracker that can post comments, `reconcile` answers each `/ask`
  exactly once (several reconcilers, or a retry after a failure, never double-answer), saying
  plainly when the document does not settle it.
- **Human code review loops back.** On `PR_OPEN`, a code reviewer's `/changes` moves the ticket
  to `PR_REWORK` with the conversation saved (the `feedback` table, per review round). The builder
  fixes on the same branch, tests and a fresh AI review run, and it is pushed to the same PR.
  Human rounds are never capped (agent-vs-agent rounds still are). Merge / decline come from the
  code host, not from comments.
- **QA is a conversation too.** After a merge, a QA owner's `/qa` starts QA. If a run fails, QA and
  the developer talk; `/fix` sends the findings (saved per QA run) into a new
  build/review/PR/merge cycle, `/qa` retests. A passing run is signed off by the QA owners.

Where conversations come from: `reconcile` reads `<comments-dir>/<KEY>/{prd,tech,pr,qa}.json`
(default `<data_dir>/comments`): `[{"id","author","created","body","reply_to"}, ...]`, and the
pull request's state from `<KEY>/pr_state` (`open|merged|declined`). A Jira / Bitbucket client
will implement the same `tracker.Tracker` (+ `Poster`, `PRWatcher`) interfaces. Who may do what is
set per Jira project under `approvers` (`prd`, `tech`, `pr`, `qa`; `"*"` is the fallback).

Typical day: `aisdlc grill PROJ-1 ...` (exits) -> people talk whenever -> `aisdlc reconcile
--once` (AI answers questions; approvals advance; `/changes` is captured) -> `aisdlc revise PROJ-1`
-> back to review, with a changelog in the thread.

## What exists / what does not (yet)
Done and tested: adapter layer, PRD grill-up, explicit-intent gates, state machine (incl. `PR_REWORK`
and the QA loop), compare-and-set store in PostgreSQL, reconciler (documents, human code review,
QA), AI answers to `/ask`, `revise`, **Jira Data Center and Bitbucket Data Center clients**, the
combined tracker (three Jira sub-task threads + the pull request), `intake`, `publish`, `doctor`,
and the local sandbox. The whole conversation is tested end to end across the simulated systems.

Not built yet:
- The stage runners that *act*: tech proposer, builder (incl. `PR_REWORK` and the post-QA fix build),
  AI reviewer, QA, and pushing the branch / opening the PR / reporting build status from the pipeline.
  The reconciler moves tickets into those states and saves the conversation they need; nothing runs
  there yet. This is the next piece.
- An asynchronous PRD interview inside Jira (today the interview runs in a terminal, then everything
  after it happens in Jira).
- Jira Cloud, AI answers on PR and QA threads, staleness detection.

## Connecting your real Jira and Bitbucket (Data Center)
1. Create a service user for the assistant and a **personal access token** for it in Jira, and an
   **HTTP access token** in Bitbucket (repository read/write). Export them as `JIRA_TOKEN` and
   `BITBUCKET_TOKEN` (the config only names the variables; never put tokens in the file).
2. Fill `jira`, `bitbucket`, `approvers` and `participants` in `aisdlc.json` (see
   `aisdlc.example.json`). Approvers and participants are Jira **usernames**.
3. Run `aisdlc doctor --issue PROJ-123 --repo PROJECT/slug`. Everything it does is read-only. If a
   check fails the message says what to look at; paste the FAIL lines if you want help.
4. Label one test ticket `ai-sdlc` and run `aisdlc intake --once`. This is the first write: it
   creates three sub-tasks and a welcome comment. Then `aisdlc grill PROJ-123` (seeds from the
   ticket and attaches the PRD), and `aisdlc reconcile` for the conversation.

The assistant's comments are authored by whoever owns the Jira token. Jira Cloud is not supported
yet (it needs the v3 API and Atlassian Document Format); `doctor` will tell you if it detects Cloud.
The Jira and Bitbucket clients are tested against simulated servers, so first contact with the real
systems may still turn up differences. `doctor` is built to surface them quickly.
