package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	maxThreadName     = 100
	maxMessageContent = 2000
	// An embed description holds far more than a message body, which is what
	// keeps a long report's trailing metadata block intact.
	maxEmbedDescription = 4096
	// Longest auto-archive window Discord offers (7 days).
	defaultAutoArchive = 10080
)

// Embed colours, matching the repo's own label colours.
const (
	colourBug         = 0xd73a4a
	colourEnhancement = 0xa2eeef
	colourDefault     = 0x6e7681
)

// truncate cuts s to at most n runes, leaving an ellipsis when it had to cut.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return strings.TrimRight(string(r[:n-1]), " \t\n") + "…"
}

// threadName renders an issue as a forum post title: "#12 Card art is wrong".
func threadName(issue Issue) string {
	return truncate(fmt.Sprintf("#%d %s", issue.Number, issue.Title), maxThreadName)
}

// threadEmbed renders a GitHub issue as the starter message of a forum post.
//
// An embed rather than plain text, because an in-game report carries its
// reporter block at the very end of the body: truncating to a message's 2000
// characters would drop precisely the metadata worth keeping, while an embed
// description holds 4096.
func threadEmbed(issue Issue) *discordgo.MessageEmbed {
	body, _ := splitReportMetadata(issue.Body)
	if body == "" {
		body = "_No description provided._"
	}
	return &discordgo.MessageEmbed{
		Title:       truncate(issue.Title, 256),
		URL:         issue.HTMLURL,
		Description: truncate(body, maxEmbedDescription),
		Color:       issueColour(issue),
		Author:      &discordgo.MessageEmbedAuthor{Name: issue.User.Login},
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("%s #%d", issueKind(issue), issue.Number),
		},
	}
}

// metadataEmbed is the thread's first reply: who filed the report and how,
// kept out of the starter message so the issue itself reads clean.
//
// It returns nil when there is nothing worth a second message.
func metadataEmbed(issue Issue) *discordgo.MessageEmbed {
	_, parsed := splitReportMetadata(issue.Body)

	fields := []*discordgo.MessageEmbedField{
		{Name: "Source", Value: issueKind(issue), Inline: true},
		{Name: "Opened by", Value: orDash(issue.User.Login), Inline: true},
	}
	if labels := issueLabels(issue); len(labels) > 0 {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name: "Labels", Value: truncate(strings.Join(labels, ", "), 1024), Inline: true,
		})
	}
	for _, f := range parsed {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   truncate(f.Name, 256),
			Value:  truncate(orDash(f.Value), 1024),
			Inline: true,
		})
	}
	// Discord caps an embed at 25 fields.
	if len(fields) > 25 {
		fields = fields[:25]
	}

	return &discordgo.MessageEmbed{
		Color:  issueColour(issue),
		Fields: fields,
		Footer: &discordgo.MessageEmbedFooter{Text: "Report details"},
	}
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func issueColour(issue Issue) int {
	for _, l := range issue.Labels {
		switch strings.ToLower(l.Name) {
		case "bug":
			return colourBug
		case "enhancement":
			return colourEnhancement
		}
	}
	return colourDefault
}

// issueKind labels the footer by where the report came from, so an in-game
// submission is distinguishable at a glance from one filed on GitHub.
func issueKind(issue Issue) string {
	for _, l := range issue.Labels {
		if strings.EqualFold(l.Name, "in-game-report") {
			return "In-game report"
		}
	}
	return "GitHub issue"
}

// issueBody is the GitHub issue body for an issue filed from a Discord thread.
func issueBody(cfg Config, thread *discordgo.Channel, author *discordgo.User, content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		content = "_No description provided._"
	}
	name := "someone"
	if author != nil {
		name = author.Username
	}
	link := fmt.Sprintf("https://discord.com/channels/%s/%s", cfg.GuildID, thread.ID)
	return fmt.Sprintf("%s\n\n---\nFiled from Discord by **%s** in [%s](%s).",
		content, name, thread.Name, link)
}

// forumThreadCreate opens a post in the forum channel and returns the thread.
func forumThreadCreate(s *discordgo.Session, channelID, name string, embed *discordgo.MessageEmbed) (*discordgo.Channel, error) {
	return s.ForumThreadStartComplex(channelID,
		&discordgo.ThreadStart{
			Name:                truncate(name, maxThreadName),
			AutoArchiveDuration: defaultAutoArchive,
		},
		&discordgo.MessageSend{
			Embeds:          []*discordgo.MessageEmbed{embed},
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
		})
}

// setThreadArchived archives or unarchives a thread. Unarchiving also clears
// the lock, otherwise only moderators could post in the revived thread.
func setThreadArchived(s *discordgo.Session, threadID string, archived, locked bool) error {
	if !archived {
		locked = false
	}
	_, err := s.ChannelEditComplex(threadID, &discordgo.ChannelEdit{
		Archived: &archived,
		Locked:   &locked,
	})
	return err
}

func renameThread(s *discordgo.Session, threadID, name string) error {
	_, err := s.ChannelEditComplex(threadID, &discordgo.ChannelEdit{
		Name: truncate(name, maxThreadName),
	})
	return err
}

// postToThread sends a plain notice into a thread with mentions disabled, so
// mirrored text can never ping the server.
func postToThread(s *discordgo.Session, threadID, content string) error {
	_, err := s.ChannelMessageSendComplex(threadID, &discordgo.MessageSend{
		Content:         truncate(content, maxMessageContent),
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	})
	return err
}

// postEmbedToThread sends an embed into a thread with mentions disabled.
func postEmbedToThread(s *discordgo.Session, threadID string, embed *discordgo.MessageEmbed) error {
	_, err := s.ChannelMessageSendComplex(threadID, &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{embed},
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	})
	return err
}

// starterMessage fetches a forum post's opening message. Discord occasionally
// reports the thread over the gateway a beat before the message is readable,
// so this retries briefly.
func starterMessage(s *discordgo.Session, threadID string) (*discordgo.Message, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		msg, err := s.ChannelMessage(threadID, threadID)
		if err == nil {
			return msg, nil
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 400 * time.Millisecond)
	}
	return nil, lastErr
}

// threadLastActivity is the time of the newest message in a thread, falling
// back to the thread's own creation time.
func threadLastActivity(th *discordgo.Channel) time.Time {
	if th.LastMessageID != "" {
		if t, err := discordgo.SnowflakeTimestamp(th.LastMessageID); err == nil {
			return t
		}
	}
	if t, err := discordgo.SnowflakeTimestamp(th.ID); err == nil {
		return t
	}
	return time.Time{}
}

// autoArchiveSlack is how close to the auto-archive deadline a manual archive
// can land before we mistake it for Discord's own timer.
const autoArchiveSlack = 5 * time.Minute

// looksAutoArchived reports whether Discord's inactivity timer, rather than a
// person, archived this thread. Discord does not flag the difference, but an
// auto-archive always lands a full AutoArchiveDuration after the last message,
// whereas a human archives while the conversation is still warm.
func looksAutoArchived(th *discordgo.Channel) bool {
	md := th.ThreadMetadata
	if md == nil || md.AutoArchiveDuration <= 0 || md.ArchiveTimestamp.IsZero() {
		return false
	}
	last := threadLastActivity(th)
	if last.IsZero() {
		return false
	}
	window := time.Duration(md.AutoArchiveDuration) * time.Minute
	return md.ArchiveTimestamp.Sub(last) >= window-autoArchiveSlack
}
