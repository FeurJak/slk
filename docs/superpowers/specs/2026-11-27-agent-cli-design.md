# Agent CLI: non-interactive Slack commands with JSON output

Date: 2026-11-27
Status: implemented on `feat/agent-cli`

## Problem

Agents need to read and write Slack as the signed-in user without the TUI.
Today the only non-interactive entry points are `slk export` (one channel to
Markdown files) and the `--dump-*` diagnostics. An agent driving slk has to
scrape a terminal UI.

## Goal

A set of subcommands that never start the TUI, print JSON on stdout, and reuse
slk's own authenticated client, token store and workspace config:

| Command | Does |
|---|---|
| `slk search <query>` | `search.messages`, Slack modifiers passed verbatim; `--count`, `--page`, `--sort score\|timestamp`, `--sort-dir` |
| `slk channels` | the conversations the user is in; `--type public,private,mpim,im`, `--member-count` |
| `slk users --query <q>` / `--id U1,U2` | id, handle, display name, real name, title, tz, is_bot, deleted |
| `slk history <channel>` | `--since`, `--until`, `--limit`, `--replies` |
| `slk thread <permalink>` / `<channel> <ts>` | the parent and every reply |
| `slk mentions` | messages that mention the user, a usergroup the user is in, or a broadcast; `--since`, `--limit` |
| `slk send <channel> <text\|->` | `--thread <ts>`, `--broadcast`, `--dry-run` |
| `slk reply <permalink> <text\|->` | `--broadcast`, `--dry-run` |
| `slk react <permalink> <emoji>` | `--dry-run` |
| `slk edit <permalink> <text\|->` | own messages only; `--dry-run` |
| `slk delete <permalink>` | own messages only; `--dry-run` |

Common to all: `--workspace` (required when more than one is configured, as for
`slk export`), JSON on stdout, errors as one JSON object on stderr with a
non-zero exit (1 for a failure, 2 for a usage error), rate limits waited out.

## Non-goals

- Listing every user in the workspace. `users.list` is deliberately absent
  from `slackclient.SlackAPI` (`TestSlackAPI_DeclaresNoWorkspaceEnumeration`):
  the walk is what gets Grid accounts signed out for scraping. `slk users`
  needs `--query` or `--id`.
- Opening conversations, joining channels, uploads, drafts.
- Any write to slk's SQLite cache. The commands do not open it at all:
  `cache.New` runs migrations, so even a "read-only" open writes.

## Design

### Where it lives

`cmd/slk/agent_*.go`, dispatched from `main` **before** `debuglog.Init`, the
same position as `--open` and for the same reason: `debuglog.Init` truncates
`slk-debug.log` in the cwd, which would erase a running TUI's log. The agent
path routes the stdlib logger to `io.Discard` (unless `SLK_DEBUG` is set,
then to stderr) so a stray `log.Printf` from shared code cannot corrupt the
JSON on stdout or stderr.

### Session

`newAgentSession` picks the token with `selectExportToken` (the `slk export`
rule), re-mints it with `remintTokens` exactly as `slk export` does, and
connects one `slackclient.Client` plus an `edge.Client` on the same
BrowserTransport. No WebSocket is started, so link chips are never looked up
(`lookupLinkChips` requires a live socket) and links are sent plain.

Enterprise Grid routing: the session holds a `workspaceRouter` with the
selected workspace in it. When the selected session `IsGridMember()` and a
command needs an org-served method (only `mentions`: `activity.feed` and
`messages.list` answer `team_is_restricted` on a member token), the other
stored tokens are connected lazily and added to the router, and the call goes
through `orgOrOwnClient` — the TUI's own rule, including the same-user
requirement. A non-Grid session never connects a second token.

### Names

Every command that prints messages resolves the user IDs it needs (authors,
DM peers, mentioned users) in one batched edge `users/info` call (the TUI's
revalidation endpoint, 80 ids per request), falling back to `users.info`
per id (`fetchUserName`, which waits out rate limits) when edge fails.
Messages carry the raw mrkdwn `text` and a `text_plain` flattened with
`messages.FlattenMrkdwnWithUserGroups` and those names.

### Writing

Text is converted the way the composer converts it:

1. `@name` / `#channel` are translated with the composer itself —
   `compose.Model.TranslateMentionsForSend` — fed only the users and
   channels the text can refer to: each distinct `@word` is looked up with
   edge `users/search` (the finder's endpoint) and registered under both its
   display name and handle; `#` loads the joined-channel list; `@` loads
   `usergroups.list`. Wire tokens (`<@U…>`) pass through untouched.
2. The translated text goes through `slackclient.ConvertMessage`
   (`mrkdwn.Convert` + `blockkit.RichTextToMrkdwn`, factored out of
   `postMessage` and `EditMessage` so the dry run shows exactly what a send
   would post).

`--dry-run` prints `{dry_run, method, channel, thread_ts, broadcast, text,
blocks}` and sends nothing. A real write prints the ts and a permalink built
locally with `slackurl.Build` (no `chat.getPermalink` call; it is what
Slack's client does, and what Grid needs anyway).

`edit` and `delete` fetch the target message first and refuse unless its
author is the signed-in user: an admin token can delete other people's
messages, and "own messages only" must not depend on the account's role.

There is no default target anywhere: every write needs its channel or
permalink spelled out; a DM is addressed as `@handle` or a user ID, which
only matches a DM that already exists.

### Rate limits

`slackclient.RetryRateLimited(ctx, fn)` retries `fn` while
`WaitOutRateLimit` says the error was a rate limit. `WaitOutRateLimit` now
also recognises `postForm`'s HTTP 429 (`*rateLimitError`), so hand-rolled
endpoints (`activity.feed`, `messages.list`) wait too. A 429 means Slack did
not act, so retrying a write after one cannot double-post.

### Times

`--since` / `--until` accept `YYYY-MM-DD` (local midnight), RFC 3339, a Slack
ts or unix seconds, or a duration back from now (`36h`, `7d`).
`since` is inclusive, `until` exclusive.

## Testing

Stdlib `testing`, white-box, against a fake TLS Slack (`newFakeSlack` /
`newTestClient`): argument parsing, the JSON shapes, mention translation,
the own-message guard, dry runs sending nothing, mentions paging stopping at
`--since`, Grid org routing, and a structural test that no agent file
imports `internal/cache`.

## Known limits

- edge `users/search` returns at most 30 users per query; a `@word` that
  matches more people than that can miss.
- edge calls (`users/info`, `users/search`, `users/counts`) are not retried
  on 429; they fall back to `users.info` where one exists.
- `mentions` reads the Activity feed, so it sees what Slack's Activity view
  sees: mentions the user's notification settings put there.
