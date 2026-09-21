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
	// Longest auto-archive window Discord offers (7 days).
	defaultAutoArchive = 10080
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

// threadBody is the starter message for a thread mirroring a GitHub issue.
func threadBody(issue Issue) string {
	body := strings.TrimSpace(issue.Body)
	if body == "" {
		body = "_No description provided._"
	}
	header := fmt.Sprintf("**%s** opened [#%d](%s) on GitHub\n\n", issue.User.Login, issue.Number, issue.HTMLURL)
	footer := "\n\n" + issue.HTMLURL
	return header + truncate(body, maxMessageContent-len([]rune(header))-len([]rune(footer))) + footer
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
func forumThreadCreate(s *discordgo.Session, channelID, name, content string) (*discordgo.Channel, error) {
	return s.ForumThreadStartComplex(channelID,
		&discordgo.ThreadStart{
			Name:                truncate(name, maxThreadName),
			AutoArchiveDuration: defaultAutoArchive,
		},
		&discordgo.MessageSend{
			Content:         truncate(content, maxMessageContent),
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
