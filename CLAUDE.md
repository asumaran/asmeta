# CLAUDE.md

Guidance for working in this repository.

## What this is

`asmeta` is a herdr plugin with no pane: herdr runs it from hooks (server
startup, `workspace.created`, `worktree.opened`, `workspace.focused`,
`workspace.renamed`) and actions, and it reports Space sidebar tokens for each
workspace (`$title`, `$desc`, `$ticket`, `$parent`, `$pr`, `$pr_state`,
`$ref`), then sorts each repo's linked worktrees by parent ticket, ticket and
PR. It is part of the `as` family of herdr plugins (asgoto, asgotopr,
asgotoissues...); asgoto searches its `$desc` token.

It started as a bash script in the user's dotfiles; the Go port keeps its
behavior byte for byte (the same tokens, the same state files, the same AI
cache keys and prompt), checked by running both against the same session.

## Stack & layout

- Go, single module, one static binary, no TUI. Only dependency: `yaml.v3`
  (through `asdevconfig.go`).
- `main.go`: CLI (`<workspace_id>`, `--all`, `--force`, `--regen`,
  `-version`), `collect` (what a workspace is: checkout, branch, ticket, PR via
  `gh pr view`, Jira), `describe`, `publish` (`workspace report-metadata`),
  `workspaceFromEnv`, `loadSecrets`.
- `jira.go`: one ticket's parent, summary and description, cached as plain
  text in `parents/`, `summaries/`, `descriptions/` of the state dir.
- `ai.go`: the descriptor: `planAI` (cache key and prompt), `cleanDescriptor`,
  `generate` (`claude -p`, Haiku), the queue (`queue/`) and its lock
  (`ai.flock`, a kernel `flock`, released however the process ends).
- `regroup.go`: the sidebar order (`regroupPlan`) and the
  `workspace.move_block` socket call.
- `ordjson.go`: JSON walked in document order, as jq's `..` does (Jira's
  document format, hook event payloads).
- Shared with the family, byte-identical (checked by `check-shared.sh`):
  `statedir.go`, `herdrbin.go`, `herdrcli.go`, `jsonfile.go`,
  `asdevconfig.go` (the asdev config and Jira credentials, shared with
  asgotoissues), `scripts/release.sh`, `.github/workflows/release.yml`. Change
  them in one repo and port the change to every copy.
- There is no `ci.yml`: the family's runs a TUI check on a pty, and asmeta
  has no TUI. The release workflow vets and tests every tag.

## State

All in the state dir (`HERDR_PLUGIN_STATE_DIR`, or the same directory worked
out by `statedir.go` when run by hand): `ai/<key>` (descriptor),
`ai/<key>.fail` (cooldown marker), `parents/`, `summaries/`, `descriptions/`
(Jira, per ticket), `queue/<workspace id>`, `last/<workspace id>` (focus
throttle), `ai.flock`. The layout is the bash plugin's; do not rename files
without a migration, or every descriptor is generated again.

## Behaviour that is not obvious

- The AI cache key is sha256 of `promptVersion`, the model and the basis (the
  PR, else the ticket, else the branch with commits). Bump `promptVersion`
  when the prompt changes. With `ASMETA_LANG` unset the prompt and the key are
  the bash plugin's; another language adds `lang=` to the basis.
- PR state: a draft PR reads `draft` whatever its state (the bash plugin's
  rule, kept for parity).
- Branches `main`, `master`, `develop` and a detached head never look a PR up.
- Focus events are throttled per workspace (`last/`); focus and rename never
  regroup.

## Build & run

```bash
go build -o asmeta .
go vet ./... && go test ./...
./asmeta --all                   # every workspace of the running server
HERDR_PLUGIN_STATE_DIR=/tmp/s ./asmeta <id>   # against a scratch state dir
```

Local dev: `herdr plugin link "$PWD"` and `go build -o asmeta .` (link does not
run `[[build]]`).

## Commits & releases

Conventional Commits; never mention AI tooling. Default branch `main`.
`scripts/release.sh <X.Y.Z>` cuts a release (only when asked).
