package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// The issues endpoint returns pull requests too; mirroring one as a bug report
// would be wrong, so they have to be filtered out.
func TestListIssuesSkipsPullRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("since"); got == "" {
			t.Errorf("expected a since cursor on the request")
		}
		w.Header().Set("ETag", `W/"abc"`)
		json.NewEncoder(w).Encode([]map[string]any{
			{"number": 1, "title": "a real issue", "state": "open"},
			{"number": 2, "title": "a pull request", "state": "open",
				"pull_request": map[string]any{"html_url": "https://example.test/pull/2"}},
		})
	}))
	defer srv.Close()

	gh := NewGitHub("token", "o", "r")
	issues, etag, notModified, err := gh.listIssues(context.Background(), srv.URL+"/issues?since=2026-01-01T00:00:00Z", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if notModified {
		t.Fatal("a 200 response must not report not-modified")
	}
	if etag != `W/"abc"` {
		t.Errorf("etag = %q", etag)
	}
	if len(issues) != 1 || issues[0].Number != 1 {
		t.Fatalf("expected only issue #1, got %+v", issues)
	}
}

// A 304 means nothing changed and costs no rate limit; it must short-circuit.
func TestListIssuesHonoursETag(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("If-None-Match") != `W/"abc"` {
			t.Errorf("If-None-Match not sent, got %q", r.Header.Get("If-None-Match"))
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	gh := NewGitHub("token", "o", "r")
	issues, etag, notModified, err := gh.listIssues(context.Background(), srv.URL+"/issues", `W/"abc"`)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !notModified || len(issues) != 0 {
		t.Fatalf("expected a not-modified result, got %d issues", len(issues))
	}
	if etag != `W/"abc"` {
		t.Errorf("etag should be preserved across a 304, got %q", etag)
	}
	if calls != 1 {
		t.Errorf("expected 1 request, got %d", calls)
	}
}

func TestListIssuesFollowsPagination(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", `<`+srv.URL+`/issues?page=2>; rel="next", <`+srv.URL+`/issues?page=2>; rel="last"`)
			json.NewEncoder(w).Encode([]map[string]any{{"number": 1, "state": "open"}})
		default:
			json.NewEncoder(w).Encode([]map[string]any{{"number": 2, "state": "closed"}})
		}
	}))
	defer srv.Close()

	gh := NewGitHub("token", "o", "r")
	issues, _, _, err := gh.listIssues(context.Background(), srv.URL+"/issues?page=1", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(issues) != 2 || issues[0].Number != 1 || issues[1].Number != 2 {
		t.Fatalf("pagination lost issues: %+v", issues)
	}
}

// Exhausting the rate limit must surface as a typed error carrying the reset
// time, so the poller can pause instead of hammering GitHub.
func TestRateLimitErrorCarriesResetTime(t *testing.T) {
	reset := time.Now().Add(15 * time.Minute).Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	gh := NewGitHub("token", "o", "r")
	_, _, _, err := gh.listIssues(context.Background(), srv.URL+"/issues", "")
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("expected a RateLimitError, got %v", err)
	}
	if !rl.Until.Equal(reset) {
		t.Errorf("reset time = %s, want %s", rl.Until, reset)
	}
}
