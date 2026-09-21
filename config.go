package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the full runtime configuration, entirely from the environment.
type Config struct {
	DiscordToken   string
	GuildID        string
	ForumChannelID string

	GitHubToken string
	RepoOwner   string
	RepoName    string

	PollInterval time.Duration
	DatabaseURL  string

	// IssueLabel is applied to issues the bot files on behalf of Discord threads.
	IssueLabel string

	// Backfill controls what happens on the very first run against an empty DB:
	// "open" imports currently-open issues, "none" starts from now.
	Backfill string

	// CloseOnArchive closes the issue when its thread is archived by a human.
	CloseOnArchive bool
	// ReopenOnUnarchive reopens the issue when its thread comes back.
	ReopenOnUnarchive bool
	// LockOnClose also locks the thread when the issue closes.
	LockOnClose bool
	// SyncTitles renames the thread when the issue title changes.
	SyncTitles bool

	DryRun bool
}

func (c Config) Repo() string { return c.RepoOwner + "/" + c.RepoName }

func LoadConfig() (Config, error) {
	c := Config{
		DiscordToken:      os.Getenv("DISCORD_TOKEN"),
		GuildID:           os.Getenv("GUILD_ID"),
		ForumChannelID:    os.Getenv("FORUM_CHANNEL_ID"),
		GitHubToken:       os.Getenv("GITHUB_TOKEN"),
		PollInterval:      envDuration("POLL_INTERVAL", 60*time.Second),
		IssueLabel:        envString("ISSUE_LABEL", "discord"),
		Backfill:          strings.ToLower(envString("BACKFILL", "open")),
		CloseOnArchive:    envBool("CLOSE_ON_ARCHIVE", true),
		ReopenOnUnarchive: envBool("REOPEN_ON_UNARCHIVE", true),
		LockOnClose:       envBool("LOCK_ON_CLOSE", false),
		SyncTitles:        envBool("SYNC_TITLES", true),
		DryRun:            envBool("DRY_RUN", false),
	}

	dsn, err := databaseURL()
	if err != nil {
		return c, err
	}
	c.DatabaseURL = dsn

	repo := os.Getenv("GITHUB_REPO")
	if owner, name, ok := strings.Cut(repo, "/"); ok && owner != "" && name != "" {
		c.RepoOwner, c.RepoName = owner, name
	}

	var missing []string
	for _, f := range []struct {
		name string
		val  string
	}{
		{"DISCORD_TOKEN", c.DiscordToken},
		{"GUILD_ID", c.GuildID},
		{"FORUM_CHANNEL_ID", c.ForumChannelID},
		{"GITHUB_TOKEN", c.GitHubToken},
		{"GITHUB_REPO (owner/name)", c.RepoOwner},
	} {
		if f.val == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	switch c.Backfill {
	case "open", "none":
	default:
		return c, fmt.Errorf("BACKFILL must be \"open\" or \"none\", got %q", c.Backfill)
	}
	if c.PollInterval < 10*time.Second {
		return c, fmt.Errorf("POLL_INTERVAL must be at least 10s, got %s", c.PollInterval)
	}
	return c, nil
}

// databaseURL prefers an explicit DATABASE_URL and otherwise assembles one
// from the discrete DB_* variables, so a password with spaces or punctuation
// survives without any quoting rules.
func databaseURL() (string, error) {
	if v := strings.TrimSpace(os.Getenv("DATABASE_URL")); v != "" {
		return v, nil
	}
	password := os.Getenv("DB_PASSWORD")
	if password == "" {
		return "", fmt.Errorf("missing required environment variables: DB_PASSWORD (or DATABASE_URL)")
	}
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(envString("DB_USER", "issue_sync"), password),
		Host:   net.JoinHostPort(envString("DB_HOST", "localhost"), envString("DB_PORT", "5432")),
		Path:   envString("DB_NAME", "issue_sync"),
	}
	q := u.Query()
	q.Set("sslmode", envString("DB_SSLMODE", "disable"))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func envString(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
