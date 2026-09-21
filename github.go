package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const githubAPI = "https://api.github.com"

// Issue is the subset of the GitHub issue payload this bot cares about.
type Issue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	// PullRequest is non-nil when this "issue" is really a PR. The issues
	// endpoint returns both; we only ever want the former.
	PullRequest *struct {
		HTMLURL string `json:"html_url"`
	} `json:"pull_request"`
}

func (i Issue) IsPullRequest() bool { return i.PullRequest != nil }

type GitHub struct {
	token  string
	owner  string
	repo   string
	client *http.Client
}

func NewGitHub(token, owner, repo string) *GitHub {
	return &GitHub{
		token:  token,
		owner:  owner,
		repo:   repo,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// RateLimitError signals that GitHub asked us to back off until Until.
type RateLimitError struct {
	Until time.Time
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("github rate limited until %s", e.Until.Format(time.RFC3339))
}

func (g *GitHub) do(ctx context.Context, method, rawurl string, body any, headers map[string]string) (*http.Response, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawurl, rdr)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gh-issue-sync")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, nil, err
	}

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if until, ok := retryAfter(resp); ok {
			return resp, data, &RateLimitError{Until: until}
		}
	}
	if resp.StatusCode >= 400 {
		return resp, data, fmt.Errorf("%s %s: %s: %s", method, rawurl, resp.Status, strings.TrimSpace(string(data)))
	}
	return resp, data, nil
}

func retryAfter(resp *http.Response) (time.Time, bool) {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return time.Now().Add(time.Duration(secs) * time.Second), true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if v := resp.Header.Get("X-RateLimit-Reset"); v != "" {
			if unix, err := strconv.ParseInt(v, 10, 64); err == nil {
				return time.Unix(unix, 0), true
			}
		}
	}
	return time.Time{}, false
}

var linkNextRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextPage(resp *http.Response) string {
	if m := linkNextRe.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
		return m[1]
	}
	return ""
}

// ListIssuesUpdatedSince returns every issue (never a PR) touched at or after
// `since`, oldest-updated first so a partial failure still advances the cursor
// safely. etag is sent as If-None-Match on the first page; a 304 means nothing
// in the repo has changed and costs nothing against the rate limit.
func (g *GitHub) ListIssuesUpdatedSince(ctx context.Context, since time.Time, etag string) ([]Issue, string, bool, error) {
	return g.listIssues(ctx, g.issuesURL(since), etag)
}

func (g *GitHub) issuesURL(since time.Time) string {
	q := url.Values{}
	q.Set("state", "all")
	q.Set("sort", "updated")
	q.Set("direction", "asc")
	q.Set("per_page", "100")
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("%s/repos/%s/%s/issues?%s", githubAPI, g.owner, g.repo, q.Encode())
}

// listIssues walks the paginated issue list starting at next.
func (g *GitHub) listIssues(ctx context.Context, next, etag string) (issues []Issue, newETag string, notModified bool, err error) {
	newETag = etag
	first := true
	for next != "" {
		headers := map[string]string{}
		if first && etag != "" {
			headers["If-None-Match"] = etag
		}
		resp, data, err := g.do(ctx, http.MethodGet, next, nil, headers)
		if err != nil {
			return nil, "", false, err
		}
		if first {
			if resp.StatusCode == http.StatusNotModified {
				return nil, etag, true, nil
			}
			if tag := resp.Header.Get("ETag"); tag != "" {
				newETag = tag
			}
			first = false
		}
		var page []Issue
		if err := json.Unmarshal(data, &page); err != nil {
			return nil, "", false, fmt.Errorf("decode issues: %w", err)
		}
		for _, is := range page {
			if is.IsPullRequest() {
				continue
			}
			issues = append(issues, is)
		}
		next = nextPage(resp)
	}
	return issues, newETag, false, nil
}

func (g *GitHub) GetIssue(ctx context.Context, number int) (*Issue, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d", githubAPI, g.owner, g.repo, number)
	_, data, err := g.do(ctx, http.MethodGet, u, nil, nil)
	if err != nil {
		return nil, err
	}
	var is Issue
	if err := json.Unmarshal(data, &is); err != nil {
		return nil, err
	}
	return &is, nil
}

func (g *GitHub) CreateIssue(ctx context.Context, title, body string, labels []string) (*Issue, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/issues", githubAPI, g.owner, g.repo)
	payload := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		payload["labels"] = labels
	}
	_, data, err := g.do(ctx, http.MethodPost, u, payload, nil)
	if err != nil {
		return nil, err
	}
	var is Issue
	if err := json.Unmarshal(data, &is); err != nil {
		return nil, err
	}
	return &is, nil
}

// SetIssueState moves an issue to "open" or "closed".
func (g *GitHub) SetIssueState(ctx context.Context, number int, state string) error {
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d", githubAPI, g.owner, g.repo, number)
	payload := map[string]any{"state": state}
	if state == "closed" {
		payload["state_reason"] = "completed"
	}
	_, _, err := g.do(ctx, http.MethodPatch, u, payload, nil)
	return err
}

func (g *GitHub) CommentOnIssue(ctx context.Context, number int, body string) error {
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", githubAPI, g.owner, g.repo, number)
	_, _, err := g.do(ctx, http.MethodPost, u, map[string]any{"body": body}, nil)
	return err
}
