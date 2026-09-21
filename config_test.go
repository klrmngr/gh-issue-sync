package main

import (
	"strings"
	"testing"
)

func TestParseForums(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     string
		single  string
		want    []ForumRoute
		wantErr string
	}{
		{
			name: "two labelled channels",
			env:  "111:bug,222:enhancement",
			want: []ForumRoute{{"111", "bug"}, {"222", "enhancement"}},
		},
		{
			name: "whitespace is forgiven",
			env:  " 111 : bug , 222 : enhancement ",
			want: []ForumRoute{{"111", "bug"}, {"222", "enhancement"}},
		},
		{
			name:   "single channel shorthand takes everything",
			single: "333",
			want:   []ForumRoute{{"333", ""}},
		},
		{
			name:    "missing label",
			env:     "111",
			wantErr: "channelID:label",
		},
		{
			name:    "empty label",
			env:     "111:",
			wantErr: "channelID:label",
		},
		{
			name:    "same channel twice",
			env:     "111:bug,111:enhancement",
			wantErr: "twice",
		},
		{
			// Two channels claiming one label would make the reverse mapping
			// (issue -> channel) ambiguous.
			name:    "same label twice",
			env:     "111:bug,222:Bug",
			wantErr: "more than one channel",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FORUM_CHANNELS", tc.env)
			t.Setenv("FORUM_CHANNEL_ID", tc.single)

			got, err := parseForums()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseForums: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("route %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestForumForIssue(t *testing.T) {
	cfg := Config{
		Forums:                []ForumRoute{{"bugs", "bug"}, {"features", "enhancement"}},
		DefaultForumChannelID: "bugs",
	}

	for _, tc := range []struct {
		name   string
		labels []string
		want   string
	}{
		{"a bug", []string{"bug", "Confirmed Reproducable"}, "bugs"},
		{"a feature request", []string{"enhancement", "good first issue"}, "features"},
		{"case does not matter", []string{"Enhancement"}, "features"},
		{"unrouted label falls back", []string{"triage"}, "bugs"},
		{"no labels at all falls back", nil, "bugs"},
		// Declaration order decides, so the mapping stays predictable.
		{"both labels take the first route", []string{"enhancement", "bug"}, "bugs"},
	} {
		if got := cfg.ForumForIssue(tc.labels); got != tc.want {
			t.Errorf("%s: ForumForIssue(%v) = %q, want %q", tc.name, tc.labels, got, tc.want)
		}
	}
}

// Without a default, an unroutable issue is skipped rather than dumped into
// whichever forum happens to be first.
func TestForumForIssueWithoutDefault(t *testing.T) {
	cfg := Config{Forums: []ForumRoute{{"bugs", "bug"}}}
	if got := cfg.ForumForIssue([]string{"triage"}); got != "" {
		t.Errorf("ForumForIssue = %q, want \"\"", got)
	}
	if got := cfg.ForumForIssue([]string{"bug"}); got != "bugs" {
		t.Errorf("ForumForIssue = %q, want bugs", got)
	}
}

func TestLabelForForum(t *testing.T) {
	cfg := Config{Forums: []ForumRoute{{"bugs", "bug"}, {"features", "enhancement"}}}

	if got := cfg.LabelForForum("features"); got != "enhancement" {
		t.Errorf("LabelForForum(features) = %q", got)
	}
	if got := cfg.LabelForForum("somewhere-else"); got != "" {
		t.Errorf("unknown channel should map to no label, got %q", got)
	}
	if !cfg.IsForum("bugs") || cfg.IsForum("somewhere-else") {
		t.Error("IsForum disagrees with the configured routes")
	}
}

func TestRouteLabels(t *testing.T) {
	cfg := Config{Forums: []ForumRoute{{"bugs", "bug"}, {"all", ""}}}
	got := cfg.RouteLabels()
	if len(got) != 1 || got[0] != "bug" {
		t.Errorf("RouteLabels = %v, want [bug]", got)
	}
}
