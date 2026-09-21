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

// ForumRoute pairs a forum channel with the GitHub label that belongs in it.
// A route with an empty Label matches nothing on its own and is only ever
// reached as the default.
type ForumRoute struct {
	ChannelID string
	Label     string
}

// Config is the full runtime configuration, entirely from the environment.
type Config struct {
	DiscordToken string
	GuildID      string

	// Forums routes issues to channels by label, in declaration order.
	Forums []ForumRoute
	// DefaultForumChannelID takes issues matching no route. Empty means such
	// issues are left alone rather than filed somewhere arbitrary.
	DefaultForumChannelID string

	GitHubToken string
	RepoOwner   string
	RepoName    string

	PollInterval time.Duration
	DatabaseURL  string

	// IssueLabel is an extra label applied to every issue filed from Discord,
	// on top of the one its forum routes to. Empty applies none.
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
	var err error
	c := Config{
		DiscordToken:      os.Getenv("DISCORD_TOKEN"),
		GuildID:           os.Getenv("GUILD_ID"),
		GitHubToken:       os.Getenv("GITHUB_TOKEN"),
		PollInterval:      envDuration("POLL_INTERVAL", 60*time.Second),
		IssueLabel:        os.Getenv("ISSUE_LABEL"),
		Backfill:          strings.ToLower(envString("BACKFILL", "open")),
		CloseOnArchive:    envBool("CLOSE_ON_ARCHIVE", true),
		ReopenOnUnarchive: envBool("REOPEN_ON_UNARCHIVE", true),
		LockOnClose:       envBool("LOCK_ON_CLOSE", false),
		SyncTitles:        envBool("SYNC_TITLES", true),
		DryRun:            envBool("DRY_RUN", false),
	}

	c.Forums, err = parseForums()
	if err != nil {
		return c, err
	}
	c.DefaultForumChannelID = strings.TrimSpace(os.Getenv("DEFAULT_FORUM_CHANNEL_ID"))
	if c.DefaultForumChannelID != "" && c.ForumRoute(c.DefaultForumChannelID) == nil {
		return c, fmt.Errorf("DEFAULT_FORUM_CHANNEL_ID %s is not one of the channels in FORUM_CHANNELS", c.DefaultForumChannelID)
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
		{"GITHUB_TOKEN", c.GitHubToken},
		{"GITHUB_REPO (owner/name)", c.RepoOwner},
		{"FORUM_CHANNELS (or FORUM_CHANNEL_ID)", firstChannel(c.Forums)},
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

// parseForums reads FORUM_CHANNELS, a comma-separated list of
// "channelID:label" pairs, e.g. "123:bug,456:enhancement". FORUM_CHANNEL_ID is
// accepted as shorthand for a single unlabelled channel that takes everything.
func parseForums() ([]ForumRoute, error) {
	raw := strings.TrimSpace(os.Getenv("FORUM_CHANNELS"))
	if raw == "" {
		if single := strings.TrimSpace(os.Getenv("FORUM_CHANNEL_ID")); single != "" {
			return []ForumRoute{{ChannelID: single}}, nil
		}
		return nil, nil
	}

	var routes []ForumRoute
	seenChannel := map[string]bool{}
	seenLabel := map[string]bool{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		id, label, ok := strings.Cut(entry, ":")
		id, label = strings.TrimSpace(id), strings.TrimSpace(label)
		if !ok || id == "" || label == "" {
			return nil, fmt.Errorf("FORUM_CHANNELS entry %q must look like channelID:label", entry)
		}
		if seenChannel[id] {
			return nil, fmt.Errorf("FORUM_CHANNELS lists channel %s twice", id)
		}
		// Two channels claiming one label would make routing order-dependent
		// and the reverse mapping ambiguous.
		if seenLabel[strings.ToLower(label)] {
			return nil, fmt.Errorf("FORUM_CHANNELS maps label %q to more than one channel", label)
		}
		seenChannel[id], seenLabel[strings.ToLower(label)] = true, true
		routes = append(routes, ForumRoute{ChannelID: id, Label: label})
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("FORUM_CHANNELS is set but lists no channels")
	}
	return routes, nil
}

func firstChannel(routes []ForumRoute) string {
	if len(routes) == 0 {
		return ""
	}
	return routes[0].ChannelID
}

// ForumRoute returns the route for a channel, or nil if it is not one of ours.
func (c Config) ForumRoute(channelID string) *ForumRoute {
	for i := range c.Forums {
		if c.Forums[i].ChannelID == channelID {
			return &c.Forums[i]
		}
	}
	return nil
}

// IsForum reports whether a channel is one the bot syncs.
func (c Config) IsForum(channelID string) bool { return c.ForumRoute(channelID) != nil }

// ForumForIssue picks the channel an issue belongs in, by the first of its
// labels that a route claims, falling back to the default channel. It returns
// "" when the issue matches nothing and no default is configured.
func (c Config) ForumForIssue(labels []string) string {
	for _, route := range c.Forums {
		if route.Label == "" {
			continue
		}
		for _, name := range labels {
			if strings.EqualFold(name, route.Label) {
				return route.ChannelID
			}
		}
	}
	return c.DefaultForumChannelID
}

// LabelForForum is the label applied to issues filed from a given channel.
func (c Config) LabelForForum(channelID string) string {
	if r := c.ForumRoute(channelID); r != nil {
		return r.Label
	}
	return ""
}

// RouteLabels lists every label used for routing, for startup validation.
func (c Config) RouteLabels() []string {
	var out []string
	for _, r := range c.Forums {
		if r.Label != "" {
			out = append(out, r.Label)
		}
	}
	return out
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
