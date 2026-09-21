package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// testStore connects to the database named by TEST_DATABASE_URL and hands back
// an empty schema. Without that variable the database-backed tests skip, so
// `go test ./...` still passes on a machine with no Postgres.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run database-backed tests")
	}
	s, err := OpenStore(dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := s.db.Exec(`TRUNCATE links, meta`); err != nil {
		t.Fatalf("reset tables: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStoreLinkRoundTrip(t *testing.T) {
	s := testStore(t)

	if _, err := s.LinkByIssue(1); !errors.Is(err, ErrNoLink) {
		t.Fatalf("missing link: got %v, want ErrNoLink", err)
	}

	in := Link{IssueNumber: 42, ThreadID: "t42", IssueState: "open", IssueTitle: "Broken", Origin: "github"}
	if err := s.PutLink(in); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := s.LinkByThread("t42")
	if err != nil {
		t.Fatalf("by thread: %v", err)
	}
	if got.IssueNumber != 42 || got.IssueState != "open" || got.Origin != "github" {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	// An update must not duplicate the row, and must not rewrite origin.
	in.IssueState = "closed"
	in.ThreadArchived = true
	in.Origin = "discord"
	if err := s.PutLink(in); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = s.LinkByIssue(42)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ThreadArchived || got.IssueState != "closed" {
		t.Fatalf("update did not stick: %+v", got)
	}
	if got.Origin != "github" {
		t.Errorf("origin should be immutable after insert, got %q", got.Origin)
	}
	if n, _ := s.CountLinks(); n != 1 {
		t.Fatalf("expected 1 link, got %d", n)
	}
}

func TestStoreWatermark(t *testing.T) {
	s := testStore(t)
	if w, err := s.Watermark(); err != nil || !w.IsZero() {
		t.Fatalf("fresh watermark = %v (%v), want zero", w, err)
	}
	want := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := s.SetWatermark(want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.Watermark()
	if err != nil || !got.Equal(want) {
		t.Fatalf("watermark = %v (%v), want %v", got, err, want)
	}
}

func TestStoreMarkThreadDeleted(t *testing.T) {
	s := testStore(t)
	if err := s.PutLink(Link{IssueNumber: 1, ThreadID: "t1", IssueState: "open"}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkThreadDeleted("t1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.LinkByIssue(1)
	if err != nil {
		t.Fatal(err)
	}
	if !got.ThreadDeleted {
		t.Fatal("thread should be marked deleted")
	}
}

func TestEchoWindowSuppressesOurOwnEdits(t *testing.T) {
	e := NewEngine(Config{}, nil, nil, nil)
	if e.isEcho("t1") {
		t.Fatal("untouched thread should not look like an echo")
	}
	e.expectEcho("t1")
	if !e.isEcho("t1") {
		t.Fatal("a thread we just edited should look like an echo")
	}
	if e.isEcho("t2") {
		t.Fatal("echo suppression leaked to another thread")
	}

	// A stale mark must not suppress a genuine human change later on.
	e.mu.Lock()
	e.echoes["t1"] = time.Now().Add(-2 * echoWindow)
	e.mu.Unlock()
	if e.isEcho("t1") {
		t.Fatal("stale echo mark should have expired")
	}
}

// A thread whose archive state already matches the store is a replay of a
// change we mirrored, and must not be pushed back to GitHub. The engine's
// GitHub client is nil here, so any call at all would panic the test.
func TestApplyThreadStateIgnoresUnchangedState(t *testing.T) {
	s := testStore(t)
	e := NewEngine(Config{CloseOnArchive: true, ReopenOnUnarchive: true}, nil, s, nil)

	link := Link{IssueNumber: 5, ThreadID: "t5", IssueState: "closed", ThreadArchived: true}
	if err := s.PutLink(link); err != nil {
		t.Fatal(err)
	}
	th := &discordgo.Channel{ID: "t5", ThreadMetadata: &discordgo.ThreadMetadata{Archived: true}}
	if err := e.applyThreadState(t.Context(), th, link); err != nil {
		t.Fatalf("applyThreadState: %v", err)
	}
}

// Discord's inactivity timer must never close an issue.
func TestApplyThreadStateIgnoresAutoArchive(t *testing.T) {
	s := testStore(t)
	e := NewEngine(Config{CloseOnArchive: true}, nil, s, nil)

	now := time.Now().UTC()
	th := &discordgo.Channel{
		ID:            snowflakeAt(now.Add(-30 * 24 * time.Hour)),
		LastMessageID: snowflakeAt(now.Add(-7 * 24 * time.Hour)),
		ThreadMetadata: &discordgo.ThreadMetadata{
			Archived:            true,
			AutoArchiveDuration: 10080,
			ArchiveTimestamp:    now,
		},
	}
	link := Link{IssueNumber: 6, ThreadID: th.ID, IssueState: "open"}
	if err := s.PutLink(link); err != nil {
		t.Fatal(err)
	}

	if err := e.applyThreadState(t.Context(), th, link); err != nil {
		t.Fatalf("applyThreadState: %v", err)
	}
	got, err := s.LinkByIssue(6)
	if err != nil {
		t.Fatal(err)
	}
	if got.IssueState != "open" {
		t.Fatalf("issue state = %q, want open", got.IssueState)
	}
	if !got.ThreadArchived {
		t.Fatal("the archive itself should still have been recorded")
	}
}

// With the mirroring switches off, nothing should reach GitHub at all.
func TestApplyThreadStateRespectsConfig(t *testing.T) {
	s := testStore(t)
	e := NewEngine(Config{CloseOnArchive: false, ReopenOnUnarchive: false}, nil, s, nil)

	now := time.Now().UTC()
	link := Link{IssueNumber: 7, ThreadID: "t7", IssueState: "open"}
	if err := s.PutLink(link); err != nil {
		t.Fatal(err)
	}
	th := &discordgo.Channel{
		ID:            "t7",
		LastMessageID: snowflakeAt(now),
		ThreadMetadata: &discordgo.ThreadMetadata{
			Archived: true, AutoArchiveDuration: 10080, ArchiveTimestamp: now,
		},
	}
	if err := e.applyThreadState(t.Context(), th, link); err != nil {
		t.Fatalf("applyThreadState: %v", err)
	}
	got, _ := s.LinkByIssue(7)
	if got.IssueState != "open" || !got.ThreadArchived {
		t.Fatalf("unexpected link after archive: %+v", got)
	}
}

// A dry run previews without persisting. If it advanced the poll cursor, the
// first real run would skip exactly the issues the preview promised.
func TestDryRunLeavesStoreUntouched(t *testing.T) {
	s := testStore(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"xyz"`)
		json.NewEncoder(w).Encode([]map[string]any{
			{"number": 11, "title": "a new bug", "state": "open",
				"updated_at": "2026-05-01T00:00:00Z",
				"labels":     []map[string]any{{"name": "bug"}}},
		})
	}))
	defer srv.Close()

	gh := NewGitHub("token", "o", "r")
	cfg := Config{
		DryRun:                true,
		Forums:                []ForumRoute{{"bugs", "bug"}},
		DefaultForumChannelID: "bugs",
	}
	e := NewEngine(cfg, gh, s, nil)

	// Drive the same path PollOnce uses, against the stub server.
	issues, etag, _, err := gh.listIssues(t.Context(), srv.URL+"/issues", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("expected 1 issue, got %d", len(issues))
	}
	// A nil discordgo session means a real thread create would panic.
	if err := e.applyIssue(t.Context(), issues[0]); err != nil {
		t.Fatalf("applyIssue: %v", err)
	}
	if !cfg.DryRun {
		t.Fatal("test misconfigured")
	}
	_ = etag

	if n, err := s.CountLinks(); err != nil || n != 0 {
		t.Errorf("dry run wrote %d links, want 0 (err %v)", n, err)
	}
	if w, err := s.Watermark(); err != nil || !w.IsZero() {
		t.Errorf("dry run moved the watermark to %v, want zero (err %v)", w, err)
	}
}

// The same call with DryRun off must persist, or the guard is too broad.
func TestNonDryRunPersistsLinks(t *testing.T) {
	s := testStore(t)
	e := NewEngine(Config{}, nil, s, nil)
	if err := e.putLink(Link{IssueNumber: 3, ThreadID: "t3", IssueState: "open"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountLinks(); n != 1 {
		t.Fatalf("expected the link to be stored, got %d", n)
	}
}
