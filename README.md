# asmeta

A herdr plugin with no pane: it fills the Space sidebar with what each
workspace is about. Each space gets two rows: `#PR descriptor` on top, then
the parent ticket, the ticket, the PR state and the branch. Inside each repo,
linked worktrees are sorted by parent ticket, ticket and PR number, so the
subtasks of one parent end up next to each other.

The descriptor is a 3 to 5 word summary, written once per PR or ticket by
`claude -p` (Haiku) and cached. Until it exists, or without `claude`, the row
shows the PR title, then the Jira summary, then the workspace label.

## Install

Requires herdr >= 0.9.0 on macOS or Linux and the GitHub CLI (`gh`) for the
PR. `claude` is optional (the AI descriptor).

```bash
herdr plugin install asumaran/asmeta
```

The install downloads the prebuilt binary of the release that matches the
manifest's version. Where there is none, it builds from source, which needs
Go. `ASMETA_BUILD_FROM_SOURCE=1` always builds from source.

The plugin runs by itself: when the server starts, when a workspace is created,
opened or renamed, and on focus (at most once every two minutes per
workspace). It also adds four actions: refresh this workspace, refresh all,
refresh pull requests, and regenerate the descriptor.

PRs are looked up for every workspace at once, in one GitHub query per
refresh, and kept in `prs.json` in the plugin's state directory. asgoto,
asgotoissues and asgotopr read that file, so the sidebar and the pickers show
the same PRs in the same state. A checkout whose `origin` is a fork finds its
PRs in the `upstream` remote.

To show the tokens, reference them in the sidebar rows of
`~/.config/herdr/config.toml`: `$title`, `$desc`, `$ticket`, `$parent`, `$pr`,
`$pr_state` and `$ref`.

## Jira

Parent tickets and summaries come from Jira, using the stacks in
`~/.claude/asdev.local.md` (YAML front matter, `stacks.<name>.jira` with
`base_url`, `email`, `api_token_env`, and optionally `type: server`,
`username` and `default_project`). The credential comes from `~/.netrc` for
the Jira host, else from the environment variable named in `api_token_env`.
Without that file, Jira is skipped. `ASDEV_CONFIG` points at another file.

## Settings

Environment variables, all optional:

| Variable | Default | Meaning |
|---|---|---|
| `ASMETA_FOCUS_THROTTLE_SECONDS` | `120` | minimum time between focus refreshes of one workspace |
| `ASMETA_SUMMARY_TTL_SECONDS` | `21600` | how long a cached Jira summary is kept |
| `ASMETA_REGROUP` | `1` | `0` leaves the worktree order alone |
| `ASMETA_CLAUDE_BIN` | `claude` on `PATH` | the CLI used for descriptors |
| `ASMETA_LANG` | Spanish | the language descriptors are written in (`English`, `Português`...) |

## License

MIT
