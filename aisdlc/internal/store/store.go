// Package store persists all pipeline state in PostgreSQL so every aisdlc process (CLI, reconciler,
// workers, on any machine) shares one central copy: tickets (state, counters, history), document
// versions, captured feedback, interview sessions and the audit log.
//
// Each ticket is one row whose JSONB `data` holds the Ticket; `state` is duplicated in its own
// column for queries and dashboards. Every write is a transaction holding that row's lock
// (SELECT ... FOR UPDATE), which makes read-modify-write cycles exclusive across processes.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"aisdlc/internal/gates"
	"aisdlc/internal/orchestrator"
)

var ErrNotFound = errors.New("ticket not found")

// keyRe restricts keys to Jira-style IDs.
var keyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)

type Artifact struct {
	Kind      string    `json:"kind"` // "prd", "tech"
	Version   int       `json:"version"`
	SHA256    string    `json:"sha256"`
	Published bool      `json:"published,omitempty"` // attached to the tracker already
	Created   time.Time `json:"created"`
}

// PRRef locates a ticket's pull request in the code host.
type PRRef struct {
	Project string `json:"project"`
	Repo    string `json:"repo"`
	ID      int    `json:"id"`
	Branch  string `json:"branch,omitempty"`
}

type HistoryEntry struct {
	At    time.Time          `json:"at"`
	From  orchestrator.State `json:"from"`
	To    orchestrator.State `json:"to"`
	Event orchestrator.Event `json:"event"`
	Note  string             `json:"note,omitempty"`
}

type Ticket struct {
	Key       string             `json:"key"`
	State     orchestrator.State `json:"state"`
	Ctx       orchestrator.Ctx   `json:"ctx"`
	Overrides map[string]string  `json:"overrides,omitempty"` // role -> adapter, per ticket
	Artifacts []Artifact         `json:"artifacts,omitempty"`
	Subtasks  map[string]string  `json:"subtasks,omitempty"` // thread ("prd","tech","qa") -> Jira sub-task key
	PR        *PRRef             `json:"pr,omitempty"`
	Replied   []string           `json:"replied,omitempty"` // "<thread>:<comment id>" of /ask comments already answered
	History   []HistoryEntry     `json:"history,omitempty"`
	Created   time.Time          `json:"created"`
	Updated   time.Time          `json:"updated"`
}

// LatestArtifact returns the highest-version artifact of a kind.
func (t *Ticket) LatestArtifact(kind string) (Artifact, bool) {
	var best Artifact
	found := false
	for _, a := range t.Artifacts {
		if a.Kind == kind && (!found || a.Version > best.Version) {
			best, found = a, true
		}
	}
	return best, found
}

type Store struct {
	db  *sql.DB
	now func() time.Time
}

// schema is applied on every Open. The advisory lock serialises concurrent first starts; a
// multi-statement Exec runs as one implicit transaction, so the lock is held until it ends.
const schema = `
SELECT pg_advisory_xact_lock(727274);
CREATE TABLE IF NOT EXISTS tickets (
	key     text PRIMARY KEY,
	state   text NOT NULL,
	data    jsonb NOT NULL,
	created timestamptz NOT NULL,
	updated timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS tickets_state ON tickets (state);
CREATE TABLE IF NOT EXISTS artifacts (
	key     text NOT NULL,
	kind    text NOT NULL,
	version int  NOT NULL,
	content bytea NOT NULL,
	PRIMARY KEY (key, kind, version)
);
CREATE TABLE IF NOT EXISTS feedback (
	key     text NOT NULL,
	kind    text NOT NULL,
	version int  NOT NULL,
	data    jsonb NOT NULL,
	saved   timestamptz NOT NULL,
	PRIMARY KEY (key, kind, version)
);
CREATE TABLE IF NOT EXISTS events (
	id     bigserial PRIMARY KEY,
	key    text NOT NULL,
	at     timestamptz NOT NULL,
	kind   text NOT NULL,
	detail jsonb
);
CREATE INDEX IF NOT EXISTS events_key ON events (key, id);
CREATE TABLE IF NOT EXISTS sessions (
	key     text PRIMARY KEY,
	data    jsonb NOT NULL,
	updated timestamptz NOT NULL
);`

// Open connects to PostgreSQL (dsn like postgres://user:pass@host:5432/aisdlc) and creates the
// tables if they do not exist yet.
func Open(dsn string) (*Store, error) { return OpenWithClock(dsn, time.Now) }

// OpenWithClock lets tests fast-forward days of human waiting without sleeping.
func OpenWithClock(dsn string, now func() time.Time) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	ctx, cancel := opCtx()
	defer cancel()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("prepare database: %w", err)
	}
	return &Store{db: db, now: now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// opCtx bounds every database call.
// shortcut: Store methods take no context (callers predate the database), upgrade when a caller needs cancellation.
func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

func ValidKey(key string) error {
	if !keyRe.MatchString(key) {
		return fmt.Errorf("invalid ticket key %q (expected like PROJ-123)", key)
	}
	return nil
}

func (s *Store) Load(key string) (*Ticket, error) {
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	ctx, cancel := opCtx()
	defer cancel()
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM tickets WHERE key = $1`, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var t Ticket
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("corrupt ticket row for %s: %w", key, err)
	}
	return &t, nil
}

func (s *Store) LoadOrCreate(key string) (*Ticket, error) {
	t, err := s.Load(key)
	if errors.Is(err, ErrNotFound) {
		return s.newTicket(key), nil
	}
	return t, err
}

func (s *Store) newTicket(key string) *Ticket {
	n := s.now().UTC()
	return &Ticket{Key: key, State: orchestrator.New, Created: n, Updated: n}
}

// errUnchanged makes update roll back without reporting an error (nothing needed writing).
var errUnchanged = errors.New("unchanged")

// update loads (or creates) the ticket with its row locked, lets fn change it, and saves it, all in
// one transaction. If fn fails nothing is written, not even a ticket that did not exist before.
func (s *Store) update(key string, fn func(ctx context.Context, tx *sql.Tx, t *Ticket) error) (*Ticket, error) {
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	ctx, cancel := opCtx()
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	fresh := s.newTicket(key)
	fb, err := json.Marshal(fresh)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tickets (key, state, data, created, updated) VALUES ($1, $2, $3, $4, $4) ON CONFLICT (key) DO NOTHING`,
		key, fresh.State, fb, fresh.Created); err != nil {
		return nil, err
	}
	var b []byte
	if err := tx.QueryRowContext(ctx, `SELECT data FROM tickets WHERE key = $1 FOR UPDATE`, key).Scan(&b); err != nil {
		return nil, err
	}
	var t Ticket
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("corrupt ticket row for %s: %w", key, err)
	}
	if err := fn(ctx, tx, &t); err != nil {
		if errors.Is(err, errUnchanged) {
			return &t, nil
		}
		return &t, err
	}
	t.Updated = s.now().UTC()
	if b, err = json.Marshal(&t); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tickets SET state = $2, data = $3, updated = $4 WHERE key = $1`, key, t.State, b, t.Updated); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &t, nil
}

// ErrStale means the ticket moved on between the moment a decision was made and the moment
// it was applied (for example "gate passed for v1" arriving after v2 exists). Callers should
// treat it as "the world changed, re-evaluate", not as a failure.
var ErrStale = errors.New("ticket changed since the decision was made")

// Expect pins the ticket state (and optionally document versions) a decision was based on.
// Zero fields are not checked.
type Expect struct {
	State       orchestrator.State
	PRDVersion  int
	TechVersion int
	PRRound     int
	QARun       int
}

// Apply loads (or creates) the ticket, runs the state machine, and persists the result plus a
// history entry. On an invalid transition nothing is written and the error is returned.
func (s *Store) Apply(key string, ev orchestrator.Event, note string, lim orchestrator.Limits) (*Ticket, error) {
	return s.ApplyExpect(key, ev, note, lim, Expect{})
}

// ApplyExpect is Apply as a compare-and-set: it only transitions if the ticket still matches
// exp, otherwise it returns ErrStale and writes nothing. Anything that decides slowly
// (fetching comments, running an agent) must use this, because a human may act meanwhile.
func (s *Store) ApplyExpect(key string, ev orchestrator.Event, note string, lim orchestrator.Limits, exp Expect) (*Ticket, error) {
	return s.update(key, func(_ context.Context, _ *sql.Tx, t *Ticket) error {
		switch {
		case exp.State != "" && t.State != exp.State,
			exp.PRDVersion != 0 && t.Ctx.PRDVersion != exp.PRDVersion,
			exp.TechVersion != 0 && t.Ctx.TechVersion != exp.TechVersion,
			exp.PRRound != 0 && t.Ctx.PRRound != exp.PRRound,
			exp.QARun != 0 && t.Ctx.QARun != exp.QARun:
			return fmt.Errorf("%w: now %s (PRD v%d, TECH v%d, PR round %d, QA run %d)", ErrStale, t.State, t.Ctx.PRDVersion, t.Ctx.TechVersion, t.Ctx.PRRound, t.Ctx.QARun)
		}
		ns, nc, err := orchestrator.Next(t.State, ev, t.Ctx, lim)
		if err != nil {
			return err
		}
		t.History = append(t.History, HistoryEntry{At: s.now().UTC(), From: t.State, To: ns, Event: ev, Note: note})
		t.State, t.Ctx = ns, nc
		return nil
	})
}

var kindRe = regexp.MustCompile(`^[a-z]+$`)

// AddArtifact stores content as the next version of its kind (one more than the highest
// existing) and records it on the ticket.
func (s *Store) AddArtifact(key, kind string, content []byte) (Artifact, error) {
	if !kindRe.MatchString(kind) {
		return Artifact{}, fmt.Errorf("invalid artifact kind %q", kind)
	}
	var a Artifact
	_, err := s.update(key, func(ctx context.Context, tx *sql.Tx, t *Ticket) error {
		version := 1
		if last, ok := t.LatestArtifact(kind); ok {
			version = last.Version + 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO artifacts (key, kind, version, content) VALUES ($1, $2, $3, $4)`, key, kind, version, content); err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		a = Artifact{Kind: kind, Version: version, SHA256: hex.EncodeToString(sum[:]), Created: s.now().UTC()}
		t.Artifacts = append(t.Artifacts, a)
		return nil
	})
	if err != nil {
		return Artifact{}, err
	}
	return a, nil
}

// ArtifactContent returns the stored document for one artifact version (ErrNotFound if none).
func (s *Store) ArtifactContent(key, kind string, version int) ([]byte, error) {
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	ctx, cancel := opCtx()
	defer cancel()
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT content FROM artifacts WHERE key = $1 AND kind = $2 AND version = $3`, key, kind, version).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

func (s *Store) List() ([]string, error) {
	ctx, cancel := opCtx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM tickets ORDER BY key COLLATE "C"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// LogEvent appends one entry to the per-ticket audit log.
func (s *Store) LogEvent(key, kind string, detail any) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	_, err = s.db.ExecContext(ctx, `INSERT INTO events (key, at, kind, detail) VALUES ($1, $2, $3, $4)`, key, s.now().UTC(), kind, b)
	return err
}

// InStateSince is when the ticket entered its current state (how long a human has had it).
func (t *Ticket) InStateSince() time.Time {
	for i := len(t.History) - 1; i >= 0; i-- {
		if t.History[i].To == t.State {
			return t.History[i].At
		}
	}
	return t.Created
}

// Feedback is everything the next revision (or rework, or fix) needs to know about a round of
// human conversation, captured at the moment it happened so it survives however long the
// follow-up takes.
type Feedback struct {
	Changes    []gates.Comment   `json:"changes"`         // explicit requests from designated people
	Discussion []gates.Comment   `json:"discussion"`      // everything else said, by anyone, in order
	Roles      map[string]string `json:"roles,omitempty"` // author -> role (product, developer, qa, ...)
}

// SaveFeedback records a round of conversation for a document version / PR round / QA run and
// returns a short reference to it for notes.
func (s *Store) SaveFeedback(key, kind string, version int, fb Feedback) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	if !kindRe.MatchString(kind) {
		return "", fmt.Errorf("invalid feedback kind %q", kind)
	}
	b, err := json.Marshal(fb)
	if err != nil {
		return "", err
	}
	ctx, cancel := opCtx()
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO feedback (key, kind, version, data, saved) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (key, kind, version) DO UPDATE SET data = EXCLUDED.data, saved = EXCLUDED.saved`, key, kind, version, b, s.now().UTC()); err != nil {
		return "", err
	}
	return fmt.Sprintf("feedback %s/%s v%d", key, kind, version), nil
}

// LoadFeedback returns the saved round (ErrNotFound if none).
func (s *Store) LoadFeedback(key, kind string, version int) (Feedback, error) {
	var fb Feedback
	if err := ValidKey(key); err != nil {
		return fb, err
	}
	if !kindRe.MatchString(kind) {
		return fb, fmt.Errorf("invalid feedback kind %q", kind)
	}
	ctx, cancel := opCtx()
	defer cancel()
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM feedback WHERE key = $1 AND kind = $2 AND version = $3`, key, kind, version).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return fb, ErrNotFound
	}
	if err != nil {
		return fb, err
	}
	if err := json.Unmarshal(b, &fb); err != nil {
		return fb, fmt.Errorf("corrupt feedback for %s %s v%d: %w", key, kind, version, err)
	}
	return fb, nil
}

// ClaimReply atomically marks an /ask comment as being answered. It returns false if someone
// (another reconciler, an earlier pass) already claimed it, so a question is answered at most
// once even with several reconcilers running. If answering then fails, call ReleaseReply.
func (s *Store) ClaimReply(key, id string) (bool, error) {
	won := false
	_, err := s.update(key, func(_ context.Context, _ *sql.Tx, t *Ticket) error {
		for _, r := range t.Replied {
			if r == id {
				return errUnchanged
			}
		}
		t.Replied = append(t.Replied, id)
		won = true
		return nil
	})
	return won && err == nil, err
}

// ReleaseReply undoes a claim after a failed attempt so the next pass retries.
func (s *Store) ReleaseReply(key, id string) error {
	_, err := s.update(key, func(_ context.Context, _ *sql.Tx, t *Ticket) error {
		keep := t.Replied[:0]
		for _, r := range t.Replied {
			if r != id {
				keep = append(keep, r)
			}
		}
		t.Replied = keep
		return nil
	})
	return err
}

// SetSubtask records the Jira sub-task that carries a thread. The first writer wins, so two
// processes that both created one cannot silently swap the mapping.
func (s *Store) SetSubtask(key, thread, subtaskKey string) (string, error) {
	got := subtaskKey
	_, err := s.update(key, func(_ context.Context, _ *sql.Tx, t *Ticket) error {
		if cur := t.Subtasks[thread]; cur != "" {
			got = cur
			return errUnchanged
		}
		if t.Subtasks == nil {
			t.Subtasks = map[string]string{}
		}
		t.Subtasks[thread] = subtaskKey
		return nil
	})
	if err != nil {
		return "", err
	}
	return got, nil
}

// SetPR records (or updates) the ticket's pull request.
func (s *Store) SetPR(key string, pr PRRef) error {
	_, err := s.update(key, func(_ context.Context, _ *sql.Tx, t *Ticket) error {
		t.PR = &pr
		return nil
	})
	return err
}

// MarkPublished flags an artifact version as attached to the tracker.
func (s *Store) MarkPublished(key, kind string, version int) error {
	_, err := s.update(key, func(_ context.Context, _ *sql.Tx, t *Ticket) error {
		for i := range t.Artifacts {
			if t.Artifacts[i].Kind == kind && t.Artifacts[i].Version == version {
				t.Artifacts[i].Published = true
				return nil
			}
		}
		return fmt.Errorf("no %s v%d artifact on %s", kind, version, key)
	})
	return err
}

// Register creates the ticket in state NEW if it does not exist yet (idempotent).
func (s *Store) Register(key string) (created bool, err error) {
	if err := ValidKey(key); err != nil {
		return false, err
	}
	t := s.newTicket(key)
	b, err := json.Marshal(t)
	if err != nil {
		return false, err
	}
	ctx, cancel := opCtx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `INSERT INTO tickets (key, state, data, created, updated) VALUES ($1, $2, $3, $4, $4) ON CONFLICT (key) DO NOTHING`,
		key, t.State, b, t.Created)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SaveSession stores an interview session (any JSON-serialisable value) for a ticket.
func (s *Store) SaveSession(key string, v any) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	_, err = s.db.ExecContext(ctx, `INSERT INTO sessions (key, data, updated) VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE SET data = EXCLUDED.data, updated = EXCLUDED.updated`, key, b, s.now().UTC())
	return err
}

// LoadSession decodes a ticket's saved interview session into v; found is false if none exists.
func (s *Store) LoadSession(key string, v any) (found bool, err error) {
	if err := ValidKey(key); err != nil {
		return false, err
	}
	ctx, cancel := opCtx()
	defer cancel()
	var b []byte
	err = s.db.QueryRowContext(ctx, `SELECT data FROM sessions WHERE key = $1`, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, fmt.Errorf("corrupt session for %s: %w", key, err)
	}
	return true, nil
}
