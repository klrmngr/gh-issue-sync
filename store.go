package main

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq"
)

// ErrNoLink is returned when no issue<->thread mapping exists.
var ErrNoLink = errors.New("no link")

// Link is one issue <-> forum thread pairing, plus the last state we
// successfully reconciled on each side. Comparing an incoming event against
// the stored state is what keeps the two directions from echoing each other.
type Link struct {
	IssueNumber    int
	ThreadID       string
	IssueState     string // "open" | "closed"
	ThreadArchived bool
	ThreadDeleted  bool
	IssueTitle     string
	IssueBody      string
	Origin         string // "github" | "discord"
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type Store struct {
	db *sql.DB
}

// The issue body is kept alongside the mapping so later triage work — dedupe,
// labelling — has the issue text locally without re-fetching from GitHub.
const schema = `
CREATE TABLE IF NOT EXISTS links (
    issue_number    INTEGER PRIMARY KEY,
    thread_id       TEXT NOT NULL UNIQUE,
    issue_state     TEXT NOT NULL DEFAULT 'open',
    thread_archived BOOLEAN NOT NULL DEFAULT FALSE,
    thread_deleted  BOOLEAN NOT NULL DEFAULT FALSE,
    issue_title     TEXT NOT NULL DEFAULT '',
    issue_body      TEXT NOT NULL DEFAULT '',
    origin          TEXT NOT NULL DEFAULT 'github',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS links_thread_id ON links(thread_id);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// OpenStore connects to Postgres and applies the schema, retrying briefly so
// the bot survives being started before the database is accepting connections.
func OpenStore(dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetConnMaxIdleTime(5 * time.Minute)

	deadline := time.Now().Add(30 * time.Second)
	for {
		err = db.Ping()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			db.Close()
			return nil, fmt.Errorf("connect to postgres: %w", err)
		}
		log.Printf("waiting for postgres: %v", err)
		time.Sleep(2 * time.Second)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func scanLink(row interface{ Scan(...any) error }) (Link, error) {
	var l Link
	err := row.Scan(&l.IssueNumber, &l.ThreadID, &l.IssueState, &l.ThreadArchived,
		&l.ThreadDeleted, &l.IssueTitle, &l.IssueBody, &l.Origin, &l.CreatedAt, &l.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNoLink
	}
	return l, err
}

const linkCols = `issue_number, thread_id, issue_state, thread_archived, thread_deleted,
                  issue_title, issue_body, origin, created_at, updated_at`

func (s *Store) LinkByIssue(number int) (Link, error) {
	return scanLink(s.db.QueryRow(`SELECT `+linkCols+` FROM links WHERE issue_number = $1`, number))
}

func (s *Store) LinkByThread(threadID string) (Link, error) {
	return scanLink(s.db.QueryRow(`SELECT `+linkCols+` FROM links WHERE thread_id = $1`, threadID))
}

// PutLink inserts or updates a mapping. origin and created_at are set once, on
// insert, and never overwritten by a later update.
func (s *Store) PutLink(l Link) error {
	_, err := s.db.Exec(`
        INSERT INTO links (issue_number, thread_id, issue_state, thread_archived,
                           thread_deleted, issue_title, issue_body, origin,
                           created_at, updated_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
        ON CONFLICT (issue_number) DO UPDATE SET
            thread_id       = EXCLUDED.thread_id,
            issue_state     = EXCLUDED.issue_state,
            thread_archived = EXCLUDED.thread_archived,
            thread_deleted  = EXCLUDED.thread_deleted,
            issue_title     = EXCLUDED.issue_title,
            issue_body      = EXCLUDED.issue_body,
            updated_at      = CURRENT_TIMESTAMP`,
		l.IssueNumber, l.ThreadID, l.IssueState, l.ThreadArchived, l.ThreadDeleted,
		l.IssueTitle, l.IssueBody, l.Origin)
	return err
}

func (s *Store) MarkThreadDeleted(threadID string) error {
	_, err := s.db.Exec(
		`UPDATE links SET thread_deleted = TRUE, updated_at = CURRENT_TIMESTAMP WHERE thread_id = $1`,
		threadID)
	return err
}

func (s *Store) GetMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES ($1, $2)
        ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, value)
	return err
}

// Watermark is the `since` cursor for issue polling: every issue updated at or
// after it still needs to be considered. Zero means "never polled".
func (s *Store) Watermark() (time.Time, error) {
	v, err := s.GetMeta("issues_since")
	if err != nil || v == "" {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, v)
}

func (s *Store) SetWatermark(t time.Time) error {
	return s.SetMeta("issues_since", t.UTC().Format(time.RFC3339))
}

func (s *Store) CountLinks() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM links`).Scan(&n)
	return n, err
}
