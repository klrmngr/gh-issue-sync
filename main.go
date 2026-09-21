package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)

	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("could not load .env: %v", err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		log.Fatal(err)
	}

	store, err := OpenStore(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer store.Close()

	gh := NewGitHub(cfg.GitHubToken, cfg.RepoOwner, cfg.RepoName)

	dg, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		log.Fatalf("discord session: %v", err)
	}
	// Guilds carries the thread lifecycle events; MessageContent is needed to
	// read the body of a forum post, which becomes the issue description.
	dg.Identify.Intents = discordgo.IntentGuilds | discordgo.IntentGuildMessages | discordgo.IntentMessageContent

	engine := NewEngine(cfg, gh, store, dg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dg.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		engine.SetBotID(r.User.ID)
		log.Printf("connected as %s", r.User.String())
	})
	dg.AddHandler(func(s *discordgo.Session, t *discordgo.ThreadCreate) { engine.OnThreadCreate(ctx, t) })
	dg.AddHandler(func(s *discordgo.Session, t *discordgo.ThreadUpdate) { engine.OnThreadUpdate(ctx, t) })
	dg.AddHandler(func(s *discordgo.Session, t *discordgo.ThreadDelete) { engine.OnThreadDelete(t) })

	if err := dg.Open(); err != nil {
		log.Fatalf("connect to discord: %v", err)
	}
	defer dg.Close()

	if err := checkForumChannels(dg, cfg); err != nil {
		log.Fatal(err)
	}
	if err := checkLabels(ctx, gh, cfg); err != nil {
		log.Fatal(err)
	}
	fresh, err := prepareWatermark(store, cfg)
	if err != nil {
		log.Fatal(err)
	}

	if cfg.DryRun {
		log.Printf("DRY_RUN is set: changes will be logged, not applied")
	}
	log.Printf("syncing %s <-> %d forum(s) every %s", cfg.Repo(), len(cfg.Forums), cfg.PollInterval)

	// On a fresh database with BACKFILL=none, existing forum posts are treated
	// as pre-existing history, exactly like the repo's existing issues.
	engine.CatchUpThreads(ctx, !fresh || cfg.Backfill != "none")
	runPollLoop(ctx, engine, cfg.PollInterval)
	log.Printf("shutting down")
}

// checkForumChannels fails fast on the most common misconfiguration: pointing
// a route at a normal text channel, or at a channel in another server.
func checkForumChannels(dg *discordgo.Session, cfg Config) error {
	for _, route := range cfg.Forums {
		ch, err := dg.Channel(route.ChannelID)
		if err != nil {
			return fmt.Errorf("forum channel %s: %w", route.ChannelID, err)
		}
		if ch.Type != discordgo.ChannelTypeGuildForum {
			return fmt.Errorf("channel %s (%s) is not a forum channel", route.ChannelID, ch.Name)
		}
		if ch.GuildID != cfg.GuildID {
			return fmt.Errorf("channel %s (%s) is not in GUILD_ID's server", route.ChannelID, ch.Name)
		}
		log.Printf("forum #%s <- label %q", ch.Name, route.Label)
	}
	return nil
}

// checkLabels verifies the routing labels exist on GitHub. A label named here
// but missing there would route nothing inbound and reject every issue the bot
// tried to file outbound, so it is worth catching at startup rather than the
// first time somebody reports a bug.
func checkLabels(ctx context.Context, gh *GitHub, cfg Config) error {
	wanted := cfg.RouteLabels()
	if cfg.IssueLabel != "" {
		wanted = append(wanted, cfg.IssueLabel)
	}
	if len(wanted) == 0 {
		return nil
	}

	existing, err := gh.ListLabels(ctx)
	if err != nil {
		return fmt.Errorf("list labels: %w", err)
	}
	var missing []string
	for _, want := range wanted {
		found := false
		for _, have := range existing {
			if strings.EqualFold(have, want) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("labels not found in %s: %s (create them, or change the config)",
			cfg.Repo(), strings.Join(missing, ", "))
	}
	return nil
}

// prepareWatermark decides what the first poll of a fresh database sees, and
// reports whether the database was in fact fresh.
func prepareWatermark(store *Store, cfg Config) (fresh bool, err error) {
	w, err := store.Watermark()
	if err != nil || !w.IsZero() {
		return false, err
	}
	if cfg.Backfill == "none" {
		log.Printf("BACKFILL=none: existing issues and forum posts will be ignored")
		return true, store.SetWatermark(time.Now())
	}
	log.Printf("BACKFILL=open: importing open issues and un-archived forum posts")
	return true, nil
}

func runPollLoop(ctx context.Context, engine *Engine, interval time.Duration) {
	poll := func() {
		if err := engine.PollOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("poll: %v", err)
		}
	}
	poll()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}
