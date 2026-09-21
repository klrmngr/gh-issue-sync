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

func TestThreadEmbedFitsDiscordLimits(t *testing.T) {
	issue := Issue{
		Number:  7,
		Title:   strings.Repeat("Overflowing ", 40),
		Body:    strings.Repeat("paragraph of detail ", 500),
		HTMLURL: "https://github.com/o/r/issues/7",
	}
	issue.User.Login = "someone"

	e := threadEmbed(issue)
	if n := len([]rune(e.Description)); n > maxEmbedDescription {
		t.Errorf("description is %d runes, limit is %d", n, maxEmbedDescription)
	}
	if n := len([]rune(e.Title)); n > 256 {
		t.Errorf("title is %d runes, limit is 256", n)
	}
	if e.URL != issue.HTMLURL {
		t.Errorf("embed lost the issue link: %q", e.URL)
	}
}

// A long in-game report must lose nothing: the prose stays in the starter
// message and the reporter block moves intact into the first reply, rather
// than being truncated off the end of a single message.
func TestLongReportSplitsWithoutLoss(t *testing.T) {
	reporter := "\n\n---\n- **Reporter:** Suspicious Entity (White)\n" +
		"- **Steam ID (unverified):** 76561198027578778\n" +
		"- **Table version:** v0.2.12"
	prose := strings.Repeat("a player describing the problem at length. ", 45)
	issue := Issue{
		Number:  99,
		Title:   "[Bug] Something broke",
		Body:    prose + reporter,
		HTMLURL: "https://github.com/o/r/issues/99",
	}
	if n := len([]rune(issue.Body)); n <= maxMessageContent {
		t.Fatalf("test body is only %d runes; it must exceed a message's %d to be meaningful", n, maxMessageContent)
	}

	starter := threadEmbed(issue)
	if !strings.Contains(starter.Description, "describing the problem at length") {
		t.Error("starter message lost the report prose")
	}
	if strings.Contains(starter.Description, "Suspicious Entity") {
		t.Error("starter message should no longer carry the reporter block")
	}

	meta := metadataEmbed(issue)
	if meta == nil {
		t.Fatal("expected a metadata reply")
	}
	joined := ""
	for _, f := range meta.Fields {
		joined += f.Name + "=" + f.Value + ";"
	}
	for _, want := range []string{"Suspicious Entity (White)", "76561198027578778", "v0.2.12"} {
		if !strings.Contains(joined, want) {
			t.Errorf("metadata reply dropped %q (fields: %s)", want, joined)
		}
	}
}

func TestThreadEmbedColourAndKind(t *testing.T) {
	bug := Issue{Labels: []struct {
		Name string `json:"name"`
	}{{Name: "bug"}, {Name: "in-game-report"}}}
	if got := issueColour(bug); got != colourBug {
		t.Errorf("bug colour = %#x, want %#x", got, colourBug)
	}
	if got := issueKind(bug); got != "In-game report" {
		t.Errorf("kind = %q, want In-game report", got)
	}

	feature := Issue{Labels: []struct {
		Name string `json:"name"`
	}{{Name: "enhancement"}}}
	if got := issueColour(feature); got != colourEnhancement {
		t.Errorf("enhancement colour = %#x", got)
	}
	if got := issueKind(feature); got != "GitHub issue" {
		t.Errorf("kind = %q, want GitHub issue", got)
	}

	if got := issueColour(Issue{}); got != colourDefault {
		t.Errorf("unlabelled colour = %#x, want %#x", got, colourDefault)
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
