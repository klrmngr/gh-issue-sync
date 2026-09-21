package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// echoWindow is how long an edit we made ourselves stays flagged, so the
// gateway event it provokes is recognised as our own and dropped.
const echoWindow = 20 * time.Second

// Engine owns both directions of the sync.
//
// Every mirrored action is decided by comparing the incoming state against the
// last state recorded in the store, so an action is only ever taken on a real
// change. That makes each direction idempotent, which in turn makes loops
// impossible: replaying a mirrored change is a no-op rather than a new event.
type Engine struct {
	cfg   Config
	gh    *GitHub
	store *Store
	dg    *discordgo.Session

	mu       sync.Mutex
	echoes   map[string]time.Time // thread ID -> when we last edited it
	botID    string
	rateWait time.Time
}

func NewEngine(cfg Config, gh *GitHub, store *Store, dg *discordgo.Session) *Engine {
	return &Engine{cfg: cfg, gh: gh, store: store, dg: dg, echoes: map[string]time.Time{}}
}

func (e *Engine) SetBotID(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.botID = id
}

// expectEcho marks a thread as about to be edited by us.
func (e *Engine) expectEcho(threadID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.echoes[threadID] = time.Now()
}

// isEcho reports whether we edited this thread moments ago, and prunes stale
// entries as it goes.
func (e *Engine) isEcho(threadID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for id, at := range e.echoes {
		if now.Sub(at) > echoWindow {
			delete(e.echoes, id)
		}
	}
	at, ok := e.echoes[threadID]
	return ok && now.Sub(at) <= echoWindow
}

// putLink records a mapping, except during a dry run, which must leave the
// store exactly as it found it.
func (e *Engine) putLink(l Link) error {
	if e.cfg.DryRun {
		return nil
	}
	return e.store.PutLink(l)
}

func (e *Engine) isSelf(userID string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return userID != "" && userID == e.botID
}

// ---------------------------------------------------------------- GitHub -> Discord

// PollOnce pulls every issue updated since the stored cursor and reconciles it.
func (e *Engine) PollOnce(ctx context.Context) error {
	e.mu.Lock()
	wait := e.rateWait
	e.mu.Unlock()
	if time.Now().Before(wait) {
		return nil
	}

	since, err := e.store.Watermark()
	if err != nil {
		return fmt.Errorf("read watermark: %w", err)
	}
	etag, err := e.store.GetMeta("issues_etag")
	if err != nil {
		return fmt.Errorf("read etag: %w", err)
	}

	issues, newETag, notModified, err := e.gh.ListIssuesUpdatedSince(ctx, since, etag)
	if err != nil {
		var rl *RateLimitError
		if errors.As(err, &rl) {
			e.mu.Lock()
			e.rateWait = rl.Until
			e.mu.Unlock()
			log.Printf("github rate limited; pausing polls until %s", rl.Until.Format(time.RFC3339))
			return nil
		}
		return err
	}
	if notModified {
		return nil
	}
	if newETag != "" && !e.cfg.DryRun {
		_ = e.store.SetMeta("issues_etag", newETag)
	}

	// Issues arrive oldest-updated first, so stopping early still leaves the
	// cursor on the last issue we actually handled.
	var high time.Time
	for _, issue := range issues {
		if err := e.applyIssue(ctx, issue); err != nil {
			log.Printf("issue #%d: %v", issue.Number, err)
			break
		}
		if issue.UpdatedAt.After(high) {
			high = issue.UpdatedAt
		}
	}
	// A dry run must not move the cursor: doing so would skip, on the next
	// real run, the very issues it just said it would mirror.
	if !high.IsZero() && !e.cfg.DryRun {
		if err := e.store.SetWatermark(high); err != nil {
			return fmt.Errorf("save watermark: %w", err)
		}
	}
	return nil
}

// applyIssue brings the Discord side in line with one GitHub issue.
func (e *Engine) applyIssue(ctx context.Context, issue Issue) error {
	link, err := e.store.LinkByIssue(issue.Number)
	switch {
	case errors.Is(err, ErrNoLink):
		// An issue that is already closed gets no thread: on first run that
		// would resurrect the whole archive of the repo.
		if issue.State != "open" {
			return nil
		}
		return e.createThreadForIssue(issue)
	case err != nil:
		return err
	}

	if link.ThreadDeleted {
		return nil
	}

	// State is applied before the title: Discord rejects every edit to an
	// archived thread except the one that un-archives it, so a reopen has to
	// land before a rename can.
	if issue.State != link.IssueState {
		closed := issue.State == "closed"
		if e.cfg.DryRun {
			log.Printf("[dry-run] issue #%d -> %s, thread %s archived=%v", issue.Number, issue.State, link.ThreadID, closed)
		} else if err := e.mirrorIssueState(link.ThreadID, issue, closed); err != nil {
			return err
		}
		link.IssueState = issue.State
		link.ThreadArchived = closed
	}

	if e.cfg.SyncTitles && issue.Title != link.IssueTitle && !link.ThreadArchived {
		name := threadName(issue)
		if e.cfg.DryRun {
			log.Printf("[dry-run] rename thread %s -> %q", link.ThreadID, name)
		} else {
			e.expectEcho(link.ThreadID)
			if err := renameThread(e.dg, link.ThreadID, name); err != nil {
				return fmt.Errorf("rename thread: %w", err)
			}
		}
		link.IssueTitle = issue.Title
	}

	link.IssueBody = issue.Body
	return e.putLink(link)
}

func (e *Engine) mirrorIssueState(threadID string, issue Issue, closed bool) error {
	e.expectEcho(threadID)
	if closed {
		// Announce before archiving; posting into an archived thread would
		// immediately revive it.
		if err := postToThread(e.dg, threadID, fmt.Sprintf("🔒 Closed on GitHub — %s", issue.HTMLURL)); err != nil {
			log.Printf("thread %s: post close notice: %v", threadID, err)
		}
		e.expectEcho(threadID)
		if err := setThreadArchived(e.dg, threadID, true, e.cfg.LockOnClose); err != nil {
			return fmt.Errorf("archive thread: %w", err)
		}
		return nil
	}

	if err := setThreadArchived(e.dg, threadID, false, false); err != nil {
		return fmt.Errorf("unarchive thread: %w", err)
	}
	e.expectEcho(threadID)
	if err := postToThread(e.dg, threadID, fmt.Sprintf("🔓 Reopened on GitHub — %s", issue.HTMLURL)); err != nil {
		log.Printf("thread %s: post reopen notice: %v", threadID, err)
	}
	return nil
}

func (e *Engine) createThreadForIssue(issue Issue) error {
	channelID := e.cfg.ForumForIssue(issueLabels(issue))
	if channelID == "" {
		// No route claims this issue and no default is set. Labelling it later
		// bumps its updated_at, so the next poll will pick it up.
		log.Printf("issue #%d: no forum matches its labels, skipping", issue.Number)
		return nil
	}
	if e.cfg.DryRun {
		log.Printf("[dry-run] create thread for issue #%d %q in channel %s", issue.Number, issue.Title, channelID)
		return nil
	}
	th, err := forumThreadCreate(e.dg, channelID, threadName(issue), threadEmbed(issue))
	if err != nil {
		return fmt.Errorf("create forum thread: %w", err)
	}
	log.Printf("issue #%d -> thread %s", issue.Number, th.ID)

	// The starter message carries the issue; its first reply carries who filed
	// it and how. Failing to post the reply must not orphan the thread, so the
	// link is still recorded below.
	if meta := metadataEmbed(issue); meta != nil {
		e.expectEcho(th.ID)
		if err := postEmbedToThread(e.dg, th.ID, meta); err != nil {
			log.Printf("thread %s: post metadata: %v", th.ID, err)
		}
	}

	return e.putLink(Link{
		IssueNumber: issue.Number,
		ThreadID:    th.ID,
		IssueState:  issue.State,
		IssueTitle:  issue.Title,
		IssueBody:   issue.Body,
		Origin:      "github",
	})
}

// ---------------------------------------------------------------- Discord -> GitHub

// OnThreadCreate files a GitHub issue for a forum post a human just opened.
func (e *Engine) OnThreadCreate(ctx context.Context, t *discordgo.ThreadCreate) {
	th := t.Channel
	if th == nil || !e.cfg.IsForum(th.ParentID) {
		return
	}
	// THREAD_CREATE also fires when the bot simply gains access to an old
	// thread; only genuinely new posts should file an issue.
	if !t.NewlyCreated && threadAge(th) > 5*time.Minute {
		return
	}
	if e.isSelf(th.OwnerID) {
		return // our own mirror of a GitHub issue
	}
	if _, err := e.store.LinkByThread(th.ID); err == nil {
		return
	} else if !errors.Is(err, ErrNoLink) {
		log.Printf("thread %s: lookup: %v", th.ID, err)
		return
	}

	if err := e.fileIssueForThread(ctx, th); err != nil {
		log.Printf("thread %s: %v", th.ID, err)
	}
}

func (e *Engine) fileIssueForThread(ctx context.Context, th *discordgo.Channel) error {
	var (
		content string
		author  *discordgo.User
	)
	if msg, err := starterMessage(e.dg, th.ID); err == nil {
		content, author = msg.Content, msg.Author
	} else {
		log.Printf("thread %s: could not read starter message: %v", th.ID, err)
	}

	if e.cfg.DryRun {
		log.Printf("[dry-run] file issue %q from thread %s", th.Name, th.ID)
		return nil
	}

	// The forum a post was opened in decides the issue's kind.
	var labels []string
	if routed := e.cfg.LabelForForum(th.ParentID); routed != "" {
		labels = append(labels, routed)
	}
	if e.cfg.IssueLabel != "" {
		labels = append(labels, e.cfg.IssueLabel)
	}
	issue, err := e.gh.CreateIssue(ctx, th.Name, issueBody(e.cfg, th, author, content), labels)
	if err != nil {
		return fmt.Errorf("create issue: %w", err)
	}
	log.Printf("thread %s -> issue #%d", th.ID, issue.Number)

	// Record the link before announcing it: once this row exists the poller
	// will recognise the issue as already mirrored and skip it.
	if err := e.putLink(Link{
		IssueNumber: issue.Number,
		ThreadID:    th.ID,
		IssueState:  issue.State,
		IssueTitle:  issue.Title,
		IssueBody:   issue.Body,
		Origin:      "discord",
	}); err != nil {
		return fmt.Errorf("save link: %w", err)
	}

	if err := postToThread(e.dg, th.ID, fmt.Sprintf("📋 Tracked as [#%d](%s)", issue.Number, issue.HTMLURL)); err != nil {
		log.Printf("thread %s: post issue link: %v", th.ID, err)
	}
	return nil
}

// OnThreadUpdate mirrors a human archiving or reviving a post.
func (e *Engine) OnThreadUpdate(ctx context.Context, t *discordgo.ThreadUpdate) {
	th := t.Channel
	if th == nil || !e.cfg.IsForum(th.ParentID) || th.ThreadMetadata == nil {
		return
	}
	link, err := e.store.LinkByThread(th.ID)
	if errors.Is(err, ErrNoLink) {
		return
	} else if err != nil {
		log.Printf("thread %s: lookup: %v", th.ID, err)
		return
	}

	if e.isEcho(th.ID) {
		// We caused this; just record where the thread ended up.
		link.ThreadArchived = th.ThreadMetadata.Archived
		if err := e.putLink(link); err != nil {
			log.Printf("thread %s: save link: %v", th.ID, err)
		}
		return
	}
	if err := e.applyThreadState(ctx, th, link); err != nil {
		log.Printf("thread %s: %v", th.ID, err)
	}
}

// applyThreadState brings the issue in line with a thread's archive state.
// It is a no-op unless the thread moved away from the state we last recorded,
// which is what stops a mirrored archive from bouncing back as a close.
func (e *Engine) applyThreadState(ctx context.Context, th *discordgo.Channel, link Link) error {
	archived := th.ThreadMetadata.Archived
	if archived == link.ThreadArchived {
		return nil
	}

	want := link.IssueState
	switch {
	case archived && e.cfg.CloseOnArchive:
		if looksAutoArchived(th) {
			log.Printf("thread %s: archived by inactivity, leaving issue #%d open", th.ID, link.IssueNumber)
		} else {
			want = "closed"
		}
	case !archived && e.cfg.ReopenOnUnarchive:
		want = "open"
	}

	if want != link.IssueState {
		if e.cfg.DryRun {
			log.Printf("[dry-run] thread %s -> issue #%d %s", th.ID, link.IssueNumber, want)
		} else if err := e.gh.SetIssueState(ctx, link.IssueNumber, want); err != nil {
			return fmt.Errorf("set issue #%d state %s: %w", link.IssueNumber, want, err)
		} else {
			log.Printf("thread %s -> issue #%d %s", th.ID, link.IssueNumber, want)
		}
		link.IssueState = want
	}

	link.ThreadArchived = archived
	return e.putLink(link)
}

// OnThreadDelete stops touching a thread that no longer exists, without
// recreating it the next time its issue is updated.
func (e *Engine) OnThreadDelete(t *discordgo.ThreadDelete) {
	if t.Channel == nil || !e.cfg.IsForum(t.Channel.ParentID) {
		return
	}
	if _, err := e.store.LinkByThread(t.Channel.ID); err != nil {
		return
	}
	if err := e.store.MarkThreadDeleted(t.Channel.ID); err != nil {
		log.Printf("thread %s: mark deleted: %v", t.Channel.ID, err)
		return
	}
	log.Printf("thread %s deleted; its issue will no longer be mirrored", t.Channel.ID)
}

func issueLabels(issue Issue) []string {
	out := make([]string, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		out = append(out, l.Name)
	}
	return out
}

func threadAge(th *discordgo.Channel) time.Duration {
	t, err := discordgo.SnowflakeTimestamp(th.ID)
	if err != nil {
		return 0
	}
	return time.Since(t)
}
