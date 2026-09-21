package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
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

	if err := checkForumChannel(dg, cfg); err != nil {
		log.Fatal(err)
	}
	fresh, err := prepareWatermark(store, cfg)
	if err != nil {
		log.Fatal(err)
	}

	if cfg.DryRun {
		log.Printf("DRY_RUN is set: changes will be logged, not applied")
	}
	log.Printf("syncing %s <-> forum %s every %s", cfg.Repo(), cfg.ForumChannelID, cfg.PollInterval)

	// On a fresh database with BACKFILL=none, existing forum posts are treated
	// as pre-existing history, exactly like the repo's existing issues.
	engine.CatchUpThreads(ctx, !fresh || cfg.Backfill != "none")
	runPollLoop(ctx, engine, cfg.PollInterval)
	log.Printf("shutting down")
}

// checkForumChannel fails fast on the most common misconfiguration: pointing
// FORUM_CHANNEL_ID at a normal text channel.
func checkForumChannel(dg *discordgo.Session, cfg Config) error {
	ch, err := dg.Channel(cfg.ForumChannelID)
	if err != nil {
		return err
	}
	if ch.Type != discordgo.ChannelTypeGuildForum {
		return errors.New("FORUM_CHANNEL_ID must be a forum channel")
	}
	if ch.GuildID != cfg.GuildID {
		return errors.New("FORUM_CHANNEL_ID is not in GUILD_ID's server")
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
