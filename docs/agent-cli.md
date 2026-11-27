# slk for agents: the JSON command line

slk has non-interactive commands that read and write Slack as the signed-in
user and print JSON. They never start the TUI, so they run beside a running
slk, in scripts and from agents. They use the workspaces you added with
`slk --add-workspace`; there is no separate login.

```
slk search 'in:#frontend from:@pablo Linear'
slk thread https://acme.slack.com/archives/C123/p1795634655404329
slk send '#deploys' 'Shipped *v1.4* — thanks @bob'
```

## The contract

- **stdout** holds exactly one JSON value when the command succeeds (exit 0).
- **stderr** holds exactly one JSON object when it fails:
  `{"error": {"kind": "...", "message": "...", "slack_error": "..."}}`.
  - `kind` is `usage` (bad arguments; exit **2**), `not_found`, `refused`
    (slk will not do it, for example deleting someone else's message),
    `slack` (Slack refused; `slack_error` holds its code, such as
    `channel_not_found`), `cancelled`, or `error`. Every kind but `usage`
    exits **1**.
- Flags may come before or after the arguments. `--` ends the flags, so
  `slk send C1 -- '-1 for that'` sends text that starts with a dash.
- `--workspace <slug|team ID|name|domain>` picks the workspace. It is
  required when more than one is configured, except for commands that take a
  permalink, where the permalink's workspace is used. A permalink from a
  workspace other than the selected one is refused unless you pass
  `--workspace`.
- Rate limits are waited out, not returned as errors.
- The commands never open slk's SQLite cache, so they never write it.
- `SLK_DEBUG=1` sends slk's internal log to stderr. That breaks the
  one-JSON-object rule for stderr, so use it only for debugging.

### Naming a conversation

Wherever a command takes `<channel>`, any of these work:

| Form | Example | Matches |
|---|---|---|
| channel name | `general`, `#general` | a channel you are in |
| conversation ID | `C0123ABCD`, `D0ATV376885` | any conversation you can read |
| person | `@bob`, `U0123ABCD` | your **existing** DM with them (slk never opens a new one) |
| permalink | `https://acme.slack.com/archives/C…/p…` | that message's conversation |

Your own self-DM is `@<your handle>`.

### Times

`--since` and `--until` take any of these:

- `2026-11-26` (local midnight)
- `2026-11-26T06:24:00+11:00` (RFC 3339)
- a Slack ts such as `1795634655.404329`, or unix seconds
- a duration back from now: `36h`, `90m`, `7d`, `2w`

`--since` is inclusive and `--until` is exclusive.

### Messages

Every command that prints messages prints them in this shape:

```json
{
  "channel_id": "CH16GTF1A",
  "channel_name": "frontend",
  "channel_type": "public",
  "ts": "1795634655.404329",
  "thread_ts": "1795634655.404329",
  "time": "2026-11-26T06:24:15+11:00",
  "user_id": "U036H75CWH1",
  "user_name": "Paweł Dzierżyński (Pablito)",
  "user_handle": "dzierzynskip",
  "text": "Hi <@U0ATH1YLTU7>, could you please confirm one thing for me? …",
  "text_plain": "Hi @Taras Woronjanski, could you please confirm one thing for me? …",
  "reply_count": 1,
  "permalink": "https://unleashweb.slack.com/archives/CH16GTF1A/p1795634655404329?thread_ts=1795634655.404329"
}
```

- `text` is Slack's mrkdwn as stored, with mentions as `<@U…>`.
- `text_plain` has names in place of IDs and links reduced to their labels.
  For a forwarded message it also includes the shared text.
- `thread_ts` is set on thread parents (equal to `ts`) and on replies.
- `reactions`, `files`, `edited`, `subtype` and `bot_id` appear when they
  apply.

## Reading

### `slk search <query>`

Server-side `search.messages`. Slack's search modifiers (`in:#chan`,
`from:@user`, `before:`, `after:`, `has:link`, …) are passed through
unchanged. Several arguments are joined into one query.

| Flag | Default | |
|---|---|---|
| `--count` | 20 | results per page, 1–100 |
| `--page` | 1 | page of results |
| `--sort` | `score` | `score` (relevance) or `timestamp` |
| `--sort-dir` | `desc` | `desc` or `asc` |

```
$ slk search 'in:#frontend from:@dzierzynskip Linear' --count 5
{
  "query": "in:#frontend from:@dzierzynskip Linear",
  "total": 11, "page": 1, "pages": 3, "count": 5,
  "matches": [ { …message… }, … ]
}
```

To read the next page, request `page + 1` while `page < pages`. A DM hit has
`channel_type: "im"` and the other person's name as `channel_name`.

### `slk channels`

Lists the conversations you are in, except archived ones.

| Flag | Default | |
|---|---|---|
| `--type` | `public,private,mpim,im` | which kinds to list |
| `--member-count` | off | also count channel members (one request per channel) |

```
$ slk channels --type im
{"channels": [
  {"id": "D0ATV376885", "name": "Taras Woronjanski", "type": "im",
   "member_count": 1, "user_id": "U0ATH1YLTU7", "user_name": "Taras Woronjanski",
   "user_handle": "taras.w", "is_self": true},
  …
]}
```

DMs carry the other person (`user_id`, `user_name`, `user_handle`), and your
self-DM has `is_self: true`. Group DMs carry `members`. DMs and group DMs
always have `member_count`; channels have it only with `--member-count`.

### `slk users (--query <text> | --id U1,U2)`

```
$ slk users --query dzierzynskip
{"users": [{"id": "U036H75CWH1", "name": "dzierzynskip",
  "display_name": "Paweł Dzierżyński (Pablito)", "real_name": "Paweł Dzierżyński",
  "title": "", "tz": "Europe/Warsaw", "is_bot": false, "deleted": false,
  "team_id": "T209LQ42Z"}]}
```

- `--query` is Slack's own people search: server-ranked, at most 30 results.
- `--id` returns users in the order you gave, and lists unknown IDs under
  `not_found`.
- There is deliberately no way to list every user. Walking the whole
  workspace is what gets Enterprise Grid accounts signed out for scraping.

### `slk history <channel>`

| Flag | Default | |
|---|---|---|
| `--since`, `--until` | none | time range |
| `--limit` | 50 | most messages, 1–1000; the **newest** in the range |
| `--replies` | off | put each thread's replies under its parent in `replies` |

```
$ slk history '#deploys' --since 7d --limit 100 --replies
{"channel": {"id": "C…", "name": "deploys", "type": "public"},
 "messages": [ …oldest first… ],
 "has_more": true}
```

When `has_more` is true, older messages exist in the range. Read them with
`--until <ts of the first message>`.

### `slk thread (<permalink> | <channel> <ts>)`

Prints `{channel, thread_ts, parent, replies}`, with replies oldest first. A
reply's permalink or ts gives you its whole thread.

### `slk mentions`

Prints messages that mention you, a usergroup you are in, or `@channel`,
`@here` or `@everyone`, newest first. They come from Slack's Activity feed,
so you get what Slack's own Activity view shows.

| Flag | Default | |
|---|---|---|
| `--since` | none | oldest mention to include |
| `--limit` | 50 | 1–500 |

Each item is a message plus these fields:

- `kind`: `user`, `usergroup`, `channel` or `everyone`
- `activity_type`: Slack's own type, such as `at_user` or
  `unjoined_channel_mention`
- `unread`

## Writing

There is no default target: every write names its conversation or message.
Use `--dry-run` to print the exact payload and send nothing.

Text is markdown, converted the way slk's composer converts it:

- `*bold*`, `_italic_`, `~strike~`, `` `code` ``, lists and quotes.
- `@display name`, `@handle`, `@usergroup` and `#channel` become real
  mentions. `@here`, `@channel` and `@everyone` work.
- `<@U…>` wire tokens pass through unchanged.

Every write result lists the @words it could not resolve under
`unresolved_mentions`; they were sent as plain text. If the list is not
empty, check it before you rely on the message.

### Linked references

`send`, `reply` and `edit` also turn bare issue and pull request references
into links. `--no-linkify` turns this off for one command.

| You write | It links to | Needs |
|---|---|---|
| `owner/repo#42` | `https://github.com/owner/repo/issues/42` | nothing |
| `repo#42` | `https://github.com/<github_org>/repo/issues/42` | `github_org` |
| `EXP-61` | `https://linear.app/<linear_workspace>/issue/EXP-61` | `linear_workspace`, `EXP` in `linear_teams` |

The text stays as you wrote it and becomes the link's label. GitHub
redirects `/issues/N` to `/pull/N` when N is a pull request.

References are never linked inside `` `code` ``, code blocks, existing
`<…>` links or mentions, `[label](url)` links, or bare URLs. A reference
must stand alone, so `x/frontend#1`, `v1.EXP-2` and `EXP-1y` stay text.
The write result lists what was linked:
`"links": [{"text": "EXP-61", "url": "https://linear.app/…/issue/EXP-61"}]`.

Configure it in `~/.config/slk/config.toml`:

```toml
[agent]
github_org = "your-org"            # owner for a bare repo#N
linear_workspace = "your-workspace" # linear.app/<workspace>
linear_teams = ["EXP", "FRO"]       # team keys whose TEAM-N ids link
```

With no `[agent]` block, only `owner/repo#N` links. This means `UTF-8` and
`ISO-8601` are never mistaken for Linear issues.

Pass `-` instead of the text to read it from stdin. This is the safe choice
for anything long or full of quotes:

```
slk send '#deploys' - <<'EOF'
Shipped *v1.4*:
- faster search
- fixed @bob's bug
EOF
```

### `slk send <channel> <text|->`

Use `--thread <ts>` to reply in a thread. Add `--broadcast` (it needs
`--thread`) to also post the reply to the channel. The command prints:

```json
{"ok": true, "action": "send", "method": "chat.postMessage",
 "channel_id": "D0ATV376885", "ts": "1795760201.336219",
 "text": "…as sent…",
 "permalink": "https://unleashweb.slack.com/archives/D0ATV376885/p1795760201336219"}
```

### `slk reply <permalink> <text|->`

Replies in the thread of the linked message. A reply's link targets that
reply's thread. Takes `--broadcast`.

### `slk react <permalink> <emoji>`

`tada` and `:tada:` both work. `--remove` takes your reaction off. Reacting
twice, or removing a reaction that is not there, still succeeds.

### `slk edit <permalink> <text|->`, `slk delete <permalink>`

These work only on your own messages. slk reads the message first and
refuses (`kind: "refused"`) if someone else wrote it, even when your account
could delete it as an admin. With `--dry-run`, `delete` runs that check and
deletes nothing.

## Recipes

**Answer a question someone asked you:**

```
link=$(slk mentions --since 1d --limit 1 | jq -r '.mentions[0].permalink')
slk thread "$link" | jq '.parent.text_plain, [.replies[].text_plain]'
slk reply "$link" 'Done — see the PR.'
```

**Follow a channel incrementally:** keep the newest `ts` you saw, then run
`slk history '#deploys' --since "$last_ts"`. `--since` is inclusive, so drop
the message whose ts equals `$last_ts`.

**Check before posting:**

```
slk send '#general' "$text" --dry-run | jq '.text, .unresolved_mentions'
```

## Limits

- `@word` lookup uses Slack's people search, which returns at most 30
  people per word. A very common name can miss the person you meant; check
  `unresolved_mentions`, or write `<@U…>`.
- One message resolves at most 10 distinct @words.
- No WebSocket is opened, so links are sent plain. The TUI's Work Object
  chips (for example a Linear issue's title) are not built.
- `mentions` sees what the Activity feed holds. A mention your notification
  settings keep out of Activity is not there.
- Names come from Slack's user cache API in batches. If that fails, slk asks
  for at most 50 users one by one and leaves the rest as IDs.
