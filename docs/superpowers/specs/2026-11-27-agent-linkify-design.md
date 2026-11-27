# Agent CLI: linkify issue and PR references

Date: 2026-11-27
Status: implemented on `feat/agent-linkify`
Extends: `2026-11-27-agent-cli-design.md`

## Problem

Agents write "fixed in frontend#9673" or "see EXP-61" in Slack messages. In
Slack those stay plain text, so readers have to search for them. People
using Slack's web client get these links from integrations or by pasting
URLs; an agent's text should read the same.

## Behaviour

`slk send`, `slk reply` and `slk edit` turn bare references into Slack
links by default. `--no-linkify` turns this off for one command.

| Written | Becomes | Needs |
|---|---|---|
| `owner/repo#N` | `<https://github.com/owner/repo/issues/N\|owner/repo#N>` | nothing |
| `repo#N` | `<https://github.com/<github_org>/repo/issues/N\|repo#N>` | `github_org` |
| `TEAM-N` | `<https://linear.app/<linear_workspace>/issue/TEAM-N\|TEAM-N>` | `linear_workspace`, and `TEAM` in `linear_teams` |

- The label is the text as written, so the message reads the same.
- `/issues/N` is used for pull requests too: GitHub redirects an issue
  number that is a PR to `/pull/N`.
- A reference is never linked inside a code span or code block, an existing
  `<…>` link or mention, a `[label](url)` link, or a bare URL. These count as
  existing links: `https://linear.app/x/issue/EXP-61` stays as it is, and so
  does `github.com/org/repo#1`.
- A reference must stand alone: the character before it may not be a
  letter, digit, `_`, `.`, `/`, `#`, `@` or `-`, and a letter, digit or `_`
  may not follow it. So `abc-1` inside `xabc-1y`, an email address, or a
  path fragment are left alone.
- A Linear id is linked only for a configured team key, so `UTF-8`,
  `ISO-8601` and `SHA-256` are not.
- Write results list what was linked under `links: [{text, url}]`, the dry
  run included.

## Configuration

```toml
[agent]
github_org = "acme"          # default owner for bare repo#N
linear_workspace = "acme"    # linear.app/<workspace>
linear_teams = ["EXP", "FRO"]
```

Nothing is hard-coded in the repo. With no `[agent]` block, only
`owner/repo#N` is linked. Values are checked when used: an owner or
workspace that is not a valid URL path segment, and a team key that is not
`[A-Z][A-Z0-9]*`, are ignored. Team keys are matched case-sensitively after
upper-casing the config.

## Where

- `internal/reflink`: the pure transform, `Linkify(text, Rules)`, with no
  I/O. It is a package of its own so the TUI composer could adopt it later.
- `internal/config`: the `Agent` section.
- `cmd/slk/agent_write.go`: applies linkify after mention translation (so
  the `<@U…>` and `<#C…|name>` tokens it makes are protected spans), before
  `slackclient.ConvertMessage`.

## Testing

Table tests for `reflink.Linkify`, covering each protected span, the
boundary rules, the config gating and invalid config values. A config
decode test. CLI tests against the fake Slack: links in a dry run, the
posted text, and `--no-linkify`.
