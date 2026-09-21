package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// snowflakeAt builds a Discord ID that decodes back to the given time.
func snowflakeAt(t time.Time) string {
	return fmt.Sprintf("%d", (t.UnixMilli()-1420070400000)<<22)
}

func TestTruncate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"way too long for this", 10, "way too l…"},
		{"trailing   space here", 11, "trailing…"},
		{"émoji ünicode", 7, "émoji…"},
	} {
		got := truncate(tc.in, tc.n)
		if got != tc.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
		if n := len([]rune(got)); n > tc.n {
			t.Errorf("truncate(%q, %d) returned %d runes", tc.in, tc.n, n)
		}
	}
}

func TestThreadNameFitsDiscordLimit(t *testing.T) {
	issue := Issue{Number: 1234, Title: strings.Repeat("long title ", 40)}
	name := threadName(issue)
	if n := len([]rune(name)); n > maxThreadName {
		t.Fatalf("thread name is %d runes, limit is %d", n, maxThreadName)
	}
	if !strings.HasPrefix(name, "#1234 ") {
		t.Fatalf("thread name lost its issue number: %q", name)
	}
}

func TestThreadBodyFitsDiscordLimit(t *testing.T) {
	issue := Issue{
		Number:  7,
		Title:   "Overflowing",
		Body:    strings.Repeat("paragraph of detail ", 500),
		HTMLURL: "https://github.com/o/r/issues/7",
	}
	issue.User.Login = "someone"

	body := threadBody(issue)
	if n := len([]rune(body)); n > maxMessageContent {
		t.Fatalf("thread body is %d runes, limit is %d", n, maxMessageContent)
	}
	// The link must survive truncation: it is the thread's only pointer back.
	if !strings.HasSuffix(body, issue.HTMLURL) {
		t.Fatalf("thread body dropped the issue link, tail was %q", tail(body, 80))
	}
}

func TestThreadBodyHandlesEmptyIssue(t *testing.T) {
	body := threadBody(Issue{Number: 1, HTMLURL: "https://example.test/1"})
	if !strings.Contains(body, "No description provided") {
		t.Fatalf("empty issue body should get a placeholder, got %q", body)
	}
}

func TestLooksAutoArchived(t *testing.T) {
	now := time.Now().UTC()
	thread := func(idleMinutes, autoArchive int) *discordgo.Channel {
		return &discordgo.Channel{
			ID:            snowflakeAt(now.Add(-48 * time.Hour)),
			LastMessageID: snowflakeAt(now.Add(-time.Duration(idleMinutes) * time.Minute)),
			ThreadMetadata: &discordgo.ThreadMetadata{
				Archived:            true,
				AutoArchiveDuration: autoArchive,
				ArchiveTimestamp:    now,
			},
		}
	}

	for _, tc := range []struct {
		name        string
		idleMinutes int
		autoArchive int
		want        bool
	}{
		{"archived seconds after the last message", 0, 1440, false},
		{"archived during an active conversation", 90, 1440, false},
		{"archived right on the one-day deadline", 1440, 1440, true},
		{"archived well past the deadline", 3000, 1440, true},
		{"archived just inside the slack window", 1436, 1440, true},
		{"archived comfortably before the deadline", 1400, 1440, false},
		{"one-hour window, timed out", 60, 60, true},
		{"one-hour window, manual", 20, 60, false},
	} {
		if got := looksAutoArchived(thread(tc.idleMinutes, tc.autoArchive)); got != tc.want {
			t.Errorf("%s: looksAutoArchived = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLooksAutoArchivedWithoutMetadata(t *testing.T) {
	if looksAutoArchived(&discordgo.Channel{ID: "1"}) {
		t.Error("a thread with no metadata must not be treated as auto-archived")
	}
	ch := &discordgo.Channel{ID: "1", ThreadMetadata: &discordgo.ThreadMetadata{Archived: true}}
	if looksAutoArchived(ch) {
		t.Error("a thread with no auto-archive duration must not be treated as auto-archived")
	}
}

func TestIssueBodyLinksBackToThread(t *testing.T) {
	cfg := Config{GuildID: "111"}
	th := &discordgo.Channel{ID: "222", Name: "Deck import fails"}
	body := issueBody(cfg, th, &discordgo.User{Username: "kai"}, "It just spins forever.")

	for _, want := range []string{"It just spins forever.", "kai", "discord.com/channels/111/222"} {
		if !strings.Contains(body, want) {
			t.Errorf("issue body missing %q:\n%s", want, body)
		}
	}
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
