package main

import (
	"context"
	"errors"
	"log"

	"github.com/bwmarrin/discordgo"
)

// CatchUpThreads reconciles the Discord side once at startup, covering
// anything that happened while the bot was offline: posts opened without an
// issue, and posts archived or revived without the gateway event reaching us.
//
// The GitHub side needs no equivalent pass — the stored `since` cursor already
// replays every issue touched during the downtime.
//
// fileNew must be false when the forum may already hold posts that predate the
// bot and were never meant to be issues; otherwise the first startup would file
// one for every last of them.
func (e *Engine) CatchUpThreads(ctx context.Context, fileNew bool) {
	threads, err := e.forumThreads()
	if err != nil {
		log.Printf("catch-up: list threads: %v", err)
		return
	}

	var filed, reconciled int
	for _, th := range threads {
		if th.ThreadMetadata == nil {
			continue
		}
		link, err := e.store.LinkByThread(th.ID)
		switch {
		case errors.Is(err, ErrNoLink):
			// Only unarchived, human-authored posts are worth filing; an old
			// archived post predates the bot and should stay where it is.
			if !fileNew || th.ThreadMetadata.Archived || e.isSelf(th.OwnerID) {
				continue
			}
			if err := e.fileIssueForThread(ctx, th); err != nil {
				log.Printf("catch-up: thread %s: %v", th.ID, err)
				continue
			}
			filed++
		case err != nil:
			log.Printf("catch-up: thread %s: lookup: %v", th.ID, err)
		default:
			if link.ThreadArchived == th.ThreadMetadata.Archived {
				continue
			}
			if err := e.applyThreadState(ctx, th, link); err != nil {
				log.Printf("catch-up: thread %s: %v", th.ID, err)
				continue
			}
			reconciled++
		}
	}
	log.Printf("catch-up: %d threads scanned, %d issues filed, %d states reconciled", len(threads), filed, reconciled)
}

// forumThreads returns the forum channel's active and recently archived posts.
func (e *Engine) forumThreads() ([]*discordgo.Channel, error) {
	// Active threads are only listable guild-wide; Discord retired the
	// per-channel endpoint.
	active, err := e.dg.GuildThreadsActive(e.cfg.GuildID)
	if err != nil {
		return nil, err
	}
	var out []*discordgo.Channel
	for _, th := range active.Threads {
		if th.ParentID == e.cfg.ForumChannelID {
			out = append(out, th)
		}
	}

	archived, err := e.dg.ThreadsArchived(e.cfg.ForumChannelID, nil, 100)
	if err != nil {
		// Not fatal: active threads alone still cover the common case.
		log.Printf("catch-up: list archived threads: %v", err)
		return out, nil
	}
	out = append(out, archived.Threads...)
	return out, nil
}
