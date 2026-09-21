# gh-issue-sync

**The Firemind** — keeps GitHub issues and a Discord forum channel in sync,
both directions.

- A new issue on GitHub opens a forum post.
- A new forum post files a GitHub issue.
- Closing an issue archives its post; archiving a post closes its issue.
- Reopening or un-archiving does the reverse.

Built for [klrmngr/mtg-edh-4player](https://github.com/klrmngr/mtg-edh-4player),
but the repo and channel are configuration, not code.

## How it stays in sync

**GitHub → Discord is polled**, not webhooked. The bot asks for every issue
updated since a stored cursor, with an `If-None-Match` header — an unchanged
repo answers `304`, which costs nothing against the rate limit. At the default
60s interval that is roughly 1,400 requests a day against a 5,000/hour budget.

Polling means no inbound network: the bot works behind NAT with no tunnel, no
open port and no webhook secret. It also means downtime is self-healing. If the
bot is off for an hour, the cursor is simply an hour old and the next poll
replays everything it missed, whereas a missed webhook delivery would have to be
redelivered by hand.

**Discord → GitHub is event-driven** over the gateway, since the bot is already
holding that connection.

### Why it doesn't loop

Every mapping row records the last state reconciled on *each* side. An action is
only taken when an incoming event differs from that stored state, so replaying a
change the bot itself made is a no-op rather than a new event. Mirroring is
therefore idempotent in both directions, which is what makes a feedback loop
impossible rather than merely unlikely.

Two smaller guards sit on top:

- Threads the bot opened are owned by the bot, so its own posts never file
  issues back to GitHub.
- Edits the bot is about to make are flagged for 20 seconds, so the gateway
  event they provoke is recognised as an echo even if it arrives before the
  database write lands.

### Auto-archive is not a close

Discord archives a quiet forum post on its own after `auto_archive_duration`,
and the gateway event looks identical to a human archiving it. Treating those
alike would silently close issues just for going quiet.

The bot separates them by timing: Discord's timer always fires a full
`auto_archive_duration` after the last message, while a person archives a post
while the conversation is still warm. An archive landing within five minutes of
that deadline is read as automatic and left alone.

## Setup

### 1. Discord application

Developer Portal → **New Application** → **Bot**:

- Name the application **The Firemind** — that is the name server members see.
  It is set in the portal, not in this repo, so it can be changed at any time.
- **Reset Token** → `DISCORD_TOKEN`
- Enable the **Message Content Intent** (needed to read a post's opening message,
  which becomes the issue body)

Invite it with these permissions — OAuth2 → URL Generator, scope `bot`, or use
`permissions=326417583104`:

| Permission | Why |
| --- | --- |
| View Channels | see the forum |
| Send Messages / Send Messages in Threads | post the issue link into a thread |
| Create Public Threads | mirror an issue as a post |
| Manage Threads | archive, un-archive and rename posts |
| Read Message History | read a post's opening message |

Create a **Forum** channel for the issues, then copy its ID (Developer Mode on,
right-click → Copy Channel ID) into `FORUM_CHANNEL_ID`. The bot refuses to start
if that ID is not a forum channel.

### 2. GitHub token

A fine-grained PAT, repository access limited to the one repo:

- **Issues** — Read and write
- **Metadata** — Read-only

### 3. Run it

```bash
cp .env.example .env   # then fill it in
docker compose up --build -d
docker compose logs -f bot
```

On the first run `BACKFILL=open` mirrors the currently-open issues and ignores
closed ones, so the channel does not fill with the repo's entire history. It
does the same in the other direction: un-archived forum posts get issues,
archived ones are left as history. Use `BACKFILL=none` to ignore both sides and
start from now. The setting only applies while the database is empty.

Try `DRY_RUN=true` first to see what a first run would do. If the forum channel
already has posts you do *not* want filed as issues, start with `BACKFILL=none`.

## Configuration

Everything is environment variables; see [.env.example](.env.example) for the
full list with defaults. The ones worth a second look:

- `REOPEN_ON_UNARCHIVE` (default `true`) — any new message in an archived post
  un-archives it in Discord, so a "thanks, fixed!" on a closed post will reopen
  the issue. Set it to `false` if that is more noise than signal.
- `LOCK_ON_CLOSE` (default `false`) — locking a post on close stops non-moderators
  reviving it, which also prevents the above.
- `ISSUE_LABEL` (default `discord`) — applied to issues filed from Discord. The
  label must already exist in the repo.

## Running without Docker

```bash
export DATABASE_URL="postgres://user:pw@localhost:5432/issue_sync?sslmode=disable"
make run
```

The schema is applied automatically at startup.

## Development

```bash
make test      # unit tests; database tests skip
make test-db   # full suite against a throwaway Postgres container
```

## Layout

| File | Contents |
| --- | --- |
| `main.go` | wiring, startup checks, poll loop |
| `config.go` | environment configuration |
| `store.go` | Postgres: the issue ↔ thread mapping and the poll cursor |
| `github.go` | GitHub REST client (list, create, open/close) |
| `discord.go` | forum-thread helpers and the auto-archive heuristic |
| `sync.go` | both directions of the reconciler |
| `catchup.go` | one startup pass over threads, for events missed while offline |

## Not synced

Comments, labels and reactions are deliberately left alone. The mapping table
keeps each issue's title and body alongside the link, which is the hook for
later triage work — duplicate detection, auto-labelling — without needing to
re-fetch from GitHub.
