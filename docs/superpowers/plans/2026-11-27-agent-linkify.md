# Agent linkify implementation plan

Design: `docs/superpowers/specs/2026-11-27-agent-linkify-design.md`.
Branch: `feat/agent-linkify`. Tests first per step.

1. `internal/reflink`: `Rules`, `Link`, `Linkify`; table tests first.
2. `internal/config`: `[agent]` → `Config.Agent` (`github_org`,
   `linear_workspace`, `linear_teams`); decode test first.
3. `cmd/slk`: `--no-linkify` on send, reply and edit; `links` in
   `agentWriteResult`; linkify after mention translation. CLI tests first.
4. Docs: `docs/agent-cli.md` (a Linkify section, the config, the flag),
   AGENTS.md rows for `reflink.Linkify` and `Config.Agent`.
5. Gates: build, vet, `test -race`, golangci-lint, gofmt.
6. Live: dry runs only, `send --dry-run` to the self-DM with references,
   code and URLs.
