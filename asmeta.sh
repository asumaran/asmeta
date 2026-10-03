#!/usr/bin/env bash
# asmeta: report what each workspace is about as herdr Space sidebar tokens.
#
#   asmeta.sh                 workspace from the herdr hook env (event / action)
#   asmeta.sh <workspace_id>  one workspace
#   asmeta.sh --all           every workspace
#   --force                   ignore the focus throttle
#   --regen                   drop the cached AI descriptor and generate it again
#
# Tokens: $title (row 1: "#PR descriptor" on a linked worktree, the workspace
# label elsewhere), $desc (the descriptor alone, what asgoto searches),
# $ticket, $parent, $pr ("#123 draft", kept for sorting), $pr_state (draft,
# merged or closed) and $ref (the branch, on the main checkout and on worktrees
# without a ticket, so row 2 always says something).
#
# Sources: branch name (ticket key), `gh pr view` (PR), Jira REST (parent,
# summary, description; stacks from ~/.claude/asdev.local.md, the same file
# asdev / asgotoissues read) and `claude -p` (Haiku) for the descriptor.
#
# The descriptor is stable: with a PR it depends on the PR and the ticket only,
# without one it is generated once from the ticket (or from the commits when
# there is no ticket either) and frozen until a PR appears. Until it exists,
# or when generation fails, it falls back to the PR title, the Jira summary,
# then the label. Every hook publishes the fallback first; AI generation goes
# through a queue drained by a single process at a time (a global lock), so
# concurrent events never pile up model calls or herdr plugin command slots.
#
# After reporting, the linked worktrees of each repo are reordered in the
# sidebar by (parent ticket, ticket, PR number) ascending, so subtasks of one
# parent sit together right under the repo row. A worktree without a parent
# groups under its own ticket, so the parent's worktree leads its children.
# ASMETA_REGROUP=0 disables it. Focus and rename events never reorder.
set -uo pipefail

SOURCE="asumaran.asmeta"
HERDR="${HERDR_BIN_PATH:-herdr}"
# The hooks get HERDR_PLUGIN_STATE_DIR; manual runs default to the same dir so
# they share the caches and the lock.
STATE_DIR="${HERDR_PLUGIN_STATE_DIR:-$HOME/.local/state/herdr/plugins/$SOURCE}"
ASDEV_CONFIG="${ASDEV_CONFIG:-$HOME/.claude/asdev.local.md}"
FOCUS_THROTTLE_SECONDS="${ASMETA_FOCUS_THROTTLE_SECONDS:-120}"
JIRA_TTL_SECONDS="${ASMETA_SUMMARY_TTL_SECONDS:-21600}"
REGROUP="${ASMETA_REGROUP:-1}"
CLAUDE_BIN="${ASMETA_CLAUDE_BIN:-$(command -v claude || echo "$HOME/.local/bin/claude")}"
AI_MODEL="haiku"
AI_TIMEOUT_SECONDS=90
AI_FAIL_COOLDOWN_SECONDS=3600
# Bump when the prompt changes: every cached descriptor is regenerated.
PROMPT_VERSION=1
# Room for the descriptor on row 1 (sidebar 44 columns, minus the state icon).
TITLE_MAX=40
TICKET_RE='[A-Z][A-Z0-9]+-[0-9]+'

log() { printf 'asmeta: %s\n' "$*" >&2; }

file_age_seconds() {
  local mtime
  mtime="$(stat -f %m "$1" 2>/dev/null || stat -c %Y "$1" 2>/dev/null)" || return 1
  echo $(( $(date +%s) - mtime ))
}

# Writes stdin to $1 through a temp file in the same dir, so a concurrent
# reader never sees half a file.
atomic_write() {
  local target="$1" tmp
  mkdir -p "$(dirname "$target")"
  tmp="$(mktemp "$target.XXXXXX")" || return 1
  cat >"$tmp" && mv -f "$tmp" "$target"
}

# Hooks inherit the server environment, which may predate ~/.secrets edits.
load_secrets() {
  # shellcheck disable=SC1090,SC1091
  [[ -f "$HOME/.secrets" ]] && source "$HOME/.secrets"
  return 0
}

# ---- Jira ----

# stack<TAB>base_url<TAB>email<TAB>token_env<TAB>default_project per Jira stack.
jira_stacks() {
  [[ -f "$ASDEV_CONFIG" ]] || return 0
  command -v yq >/dev/null 2>&1 || { log "yq not installed; skipping Jira"; return 0; }
  yq -r 'select(di==0) | .stacks | to_entries[]
    | select(.value.jira.base_url != null)
    | [.key, .value.jira.base_url, .value.jira.email, .value.jira.api_token_env, (.value.jira.default_project // "")]
    | @tsv' "$ASDEV_CONFIG" 2>/dev/null
}

# Prints the issue JSON (parent, summary, description) from the first stack
# that has the ticket; nothing when no stack does.
jira_issue() {
  local ticket="$1"
  local project="${ticket%%-*}" stacks preferred="" rest=""
  stacks="$(jira_stacks)"
  [[ -n "$stacks" ]] || return 0
  # Try the stack whose default project matches first, then the others.
  while IFS=$'\t' read -r name url email token_env default_project; do
    [[ -n "$name" ]] || continue
    if [[ "$default_project" == "$project" ]]; then
      preferred+="$name"$'\t'"$url"$'\t'"$email"$'\t'"$token_env"$'\n'
    else
      rest+="$name"$'\t'"$url"$'\t'"$email"$'\t'"$token_env"$'\n'
    fi
  done <<<"$stacks"
  local body status
  body="$(mktemp)"
  while IFS=$'\t' read -r name url email token_env; do
    [[ -n "$name" ]] || continue
    local token="${!token_env:-}"
    [[ -n "$token" ]] || continue
    status="$(curl -sS -o "$body" -w '%{http_code}' --max-time 8 \
      -u "$email:$token" -H 'Accept: application/json' \
      "$url/rest/api/3/issue/$ticket?fields=parent,summary,description" 2>/dev/null)"
    case "$status" in
      200)
        cat "$body"
        rm -f "$body"
        return 0
        ;;
      404) continue ;;                       # not in this stack, try the next one
      *) log "jira $name returned $status for $ticket"; continue ;;
    esac
  done <<<"${preferred}${rest}"
  rm -f "$body"
  return 0
}

# Fetches the ticket and caches its parent (forever: a ticket's parent does
# not change), its summary and its description as plain text (both refreshed
# after JIRA_TTL_SECONDS).
jira_fetch() {
  local ticket="$1" json
  json="$(jira_issue "$ticket")"
  [[ -n "$json" ]] || return 0
  jq -r '[.fields.parent.key // "", .fields.parent.fields.issuetype.name // ""] | @tsv' \
    <<<"$json" | atomic_write "$STATE_DIR/parents/$ticket"
  jq -r '.fields.summary // ""' <<<"$json" | atomic_write "$STATE_DIR/summaries/$ticket"
  # The description is Atlassian Document Format: keep its text nodes.
  jq -r '[.fields.description | .. | .text? // empty] | join(" ") | .[0:1500]' \
    <<<"$json" | atomic_write "$STATE_DIR/descriptions/$ticket"
}

# Prints "PARENT_KEY<TAB>PARENT_TYPE" (empty when the ticket has no parent).
jira_parent() {
  local ticket="$1" cache="$STATE_DIR/parents/$1"
  [[ -f "$cache" ]] || jira_fetch "$ticket"
  [[ -f "$cache" ]] && cat "$cache"
  return 0
}

# Refreshes the summary and description caches when either is missing or old.
jira_text_fresh() {
  local ticket="$1" age
  age="$(file_age_seconds "$STATE_DIR/summaries/$ticket" 2>/dev/null)" || age=""
  if [[ -z "$age" || "$age" -ge "$JIRA_TTL_SECONDS" || ! -f "$STATE_DIR/descriptions/$ticket" ]]; then
    jira_fetch "$ticket"
  fi
}

# ---- what a workspace is ----

# Strips what the other tokens already say from a title: the Conventional
# Commits prefix ("fix(eshop|seo): ") and a leading ticket key ("ESHOP-2562 ",
# "ESHOP-2567, ").
describe() {
  sed -E -e 's/^[a-z]+(\([^)]*\))?!?:[[:space:]]*//' \
    -e "s/^\[?${TICKET_RE}\]?[,:]?[[:space:]]*//" <<<"$1"
}

# collect fills the W_* globals for one workspace of a `workspace list` JSON.
# Everything runs against the checkout with git -C: hooks start in the
# plugin's directory.
collect() {
  local list="$1" id="$2" row
  row="$(jq -c --arg id "$id" '.result.workspaces[] | select(.workspace_id == $id)' <<<"$list")"
  W_ID="$id"
  W_LABEL="$(jq -r '.label // ""' <<<"$row")"
  W_PATH="$(jq -r '.worktree.checkout_path // ""' <<<"$row")"
  W_LINKED="$(jq -r '.worktree.is_linked_worktree // false' <<<"$row")"
  W_BRANCH="" W_TICKET="" W_PARENT="" W_PR_NUM="" W_PR_LABEL="" W_PR_STATE=""
  W_PR_TITLE="" W_PR_BODY="" W_PR_BASE="" W_JIRA_SUMMARY="" W_JIRA_DESC=""
  if [[ -n "$row" && -z "$W_PATH" ]]; then
    # herdr reported no worktree metadata (the space predates its git
    # discovery): the first pane's cwd still tells which checkout it is.
    local cwd git_dir common_dir
    cwd="$("$HERDR" pane list 2>/dev/null | jq -r --arg id "$id" \
      'first(.result.panes[] | select(.workspace_id == $id) | .cwd // empty) // ""')"
    if [[ -n "$cwd" ]] && W_PATH="$(git -C "$cwd" rev-parse --show-toplevel 2>/dev/null)"; then
      git_dir="$(git -C "$W_PATH" rev-parse --absolute-git-dir 2>/dev/null)"
      common_dir="$(cd "$W_PATH" && cd "$(git rev-parse --git-common-dir 2>/dev/null)" 2>/dev/null && pwd -P)"
      [[ -n "$git_dir" && "$git_dir" != "$common_dir" ]] && W_LINKED=true
    else
      W_PATH=""
    fi
  fi
  [[ -n "$row" && -n "$W_PATH" && -d "$W_PATH" ]] || { W_PATH=""; return 0; }

  W_BRANCH="$(git -C "$W_PATH" branch --show-current 2>/dev/null)"
  W_TICKET="$(grep -oE "$TICKET_RE" <<<"$W_BRANCH" | head -1)"
  case "$W_BRANCH" in
    ""|main|master|develop) ;;               # a PR with that head is someone else's release train
    *)
      local json
      json="$(cd "$W_PATH" && gh pr view --json number,state,isDraft,title,body,baseRefName 2>/dev/null)" || json=""
      if [[ -n "$json" ]]; then
        # One field per jq call: read with a tab IFS would collapse an empty
        # state (an open PR) and shift the fields after it.
        W_PR_NUM="$(jq -r '.number' <<<"$json")"
        W_PR_STATE="$(jq -r 'if .isDraft then "draft" elif .state == "MERGED" then "merged"
          elif .state == "CLOSED" then "closed" else "" end' <<<"$json")"
        W_PR_BASE="$(jq -r '.baseRefName // ""' <<<"$json")"
        W_PR_TITLE="$(jq -r '.title' <<<"$json")"
        W_PR_BODY="$(jq -r '(.body // "") | .[0:1500]' <<<"$json")"
        W_PR_LABEL="#$W_PR_NUM${W_PR_STATE:+ $W_PR_STATE}"
      fi
      ;;
  esac
  if [[ -n "$W_TICKET" ]]; then
    local parent_type
    IFS=$'\t' read -r W_PARENT parent_type <<<"$(jira_parent "$W_TICKET")"
    # An epic is a container, not a sibling group: only real parents are shown.
    [[ "$parent_type" == "Epic" ]] && W_PARENT=""
    jira_text_fresh "$W_TICKET"
    W_JIRA_SUMMARY="$(cat "$STATE_DIR/summaries/$W_TICKET" 2>/dev/null)"
    W_JIRA_DESC="$(cat "$STATE_DIR/descriptions/$W_TICKET" 2>/dev/null)"
  fi
}

# Subjects of the commits the branch adds over its base (the PR's, else the
# remote default branch); nothing when the base cannot be resolved.
branch_commits() {
  local base="origin/HEAD"
  [[ -n "$W_PR_BASE" ]] && base="origin/$W_PR_BASE"
  git -C "$W_PATH" rev-parse --verify -q "$base" >/dev/null 2>&1 || return 0
  git -C "$W_PATH" log -20 --format=%s "$base..HEAD" 2>/dev/null
}

# ---- AI descriptor ----

# ai_plan sets AI_KEY (cache key; empty = no AI for this workspace), AI_LIMIT
# (max characters) and AI_PROMPT from the W_* globals. What goes into the key
# is what may change the descriptor: the PR (or, without one, the ticket), so
# new commits never reword it.
ai_plan() {
  AI_KEY="" AI_LIMIT="$TITLE_MAX" AI_PROMPT=""
  [[ "$W_LINKED" == "true" && -n "$W_PATH" ]] || return 0
  local basis commits=""
  if [[ -n "$W_PR_NUM" ]]; then
    basis="pr"$'\n'"$W_PR_TITLE"$'\n'"$W_PR_BODY"$'\n'"$W_JIRA_SUMMARY"$'\n'"$W_JIRA_DESC"
    local prefix="#$W_PR_NUM "
    AI_LIMIT=$(( TITLE_MAX - ${#prefix} ))
  elif [[ -n "$W_TICKET" ]]; then
    basis="ticket"$'\n'"$W_BRANCH"$'\n'"$W_TICKET"$'\n'"$W_JIRA_SUMMARY"$'\n'"$W_JIRA_DESC"
    commits="$(branch_commits)"
  else
    commits="$(branch_commits)"
    # A branch name alone says nothing the label does not: wait for commits.
    [[ -n "$commits" ]] || return 0
    basis="branch"$'\n'"$W_BRANCH"$'\n'"has_commits=1"
  fi
  AI_KEY="$(printf '%s\n%s\n%s\n' "$PROMPT_VERSION" "$AI_MODEL" "$basis" | shasum -a 256 | cut -c1-64)"

  AI_PROMPT="Escribe un descriptor muy corto, en español, de qué trata este trabajo de desarrollo. Se muestra en una lista de worktrees para reconocerlo de un vistazo.
Reglas: de 3 a 5 palabras y como máximo $AI_LIMIT caracteres. Sin número de ticket ni de PR, sin prefijos como feat o fix, sin comillas ni punto final. Empieza con mayúscula. Responde solo con el descriptor.

Branch: $W_BRANCH"
  [[ -n "$W_PR_TITLE" ]] && AI_PROMPT+=$'\n'"Título del PR: $W_PR_TITLE"
  [[ -n "$W_PR_BODY" ]] && AI_PROMPT+=$'\n'"Descripción del PR: $W_PR_BODY"
  [[ -n "$W_JIRA_SUMMARY" ]] && AI_PROMPT+=$'\n'"Ticket $W_TICKET: $W_JIRA_SUMMARY"
  [[ -n "$W_JIRA_DESC" ]] && AI_PROMPT+=$'\n'"Descripción del ticket: $W_JIRA_DESC"
  [[ -n "$commits" ]] && AI_PROMPT+=$'\n'"Commits:"$'\n'"$commits"
  return 0
}

ai_cache() { echo "$STATE_DIR/ai/$1"; }

# True when generation failed for the key less than the cooldown ago.
ai_cooling() {
  local age
  age="$(file_age_seconds "$(ai_cache "$1").fail" 2>/dev/null)" || return 1
  [[ "$age" -lt "$AI_FAIL_COOLDOWN_SECONDS" ]]
}

# First non-empty line of the model's answer, without quotes, markdown or a
# final period, cut at a word boundary to $1 characters.
clean_descriptor() {
  perl -CSD -Mutf8 -e '
    my $max = shift;
    my ($line) = grep { /\S/ } <STDIN>;
    $line //= "";
    $line =~ s/^\s+|\s+$//g;
    $line =~ s/^[\s"“”«»`*_\x27-]+|[\s"“”«»`*_\x27.]+$//g;
    while (length($line) > $max && $line =~ /\s/) { $line =~ s/\s+\S+$// }
    $line = substr($line, 0, $max) if length($line) > $max;
    print $line;
  ' "$1"
}

# Generates the descriptor for AI_KEY and caches it; on failure leaves a
# .fail marker so later events wait out the cooldown.
ai_generate() {
  local cache answer desc
  cache="$(ai_cache "$AI_KEY")"
  answer="$(MOSHI_SOCKET_PATH=/dev/null/moshi.sock \
    perl -e 'alarm shift; exec @ARGV' "$AI_TIMEOUT_SECONDS" \
    "$CLAUDE_BIN" -p --safe-mode --model "$AI_MODEL" --tools "" --no-session-persistence \
    --settings '{"disableAllHooks":true}' <<<"$AI_PROMPT" 2>/dev/null)"
  desc="$(clean_descriptor "$AI_LIMIT" <<<"$answer")"
  if [[ -z "$desc" ]]; then
    log "$W_ID ai descriptor failed for ${W_BRANCH:-?}"
    : | atomic_write "$cache.fail"
    return 1
  fi
  printf '%s\n' "$desc" | atomic_write "$cache"
  rm -f "$cache.fail"
  log "$W_ID ai descriptor: $desc"
}

# ---- publishing ----

# Reports the tokens from the W_* globals and the AI cache for AI_KEY.
publish() {
  local args=() title="" desc="" ref="" cached=""
  if [[ -z "$W_PATH" ]]; then
    # No checkout: just the name.
    "$HERDR" workspace report-metadata "$W_ID" --source "$SOURCE" --token "title=$W_LABEL" \
      --clear-token desc --clear-token ticket --clear-token parent --clear-token pr \
      --clear-token pr_state --clear-token ref >/dev/null 2>&1
    return 0
  fi
  if [[ "$W_LINKED" == "true" ]]; then
    [[ -n "$AI_KEY" && -f "$(ai_cache "$AI_KEY")" ]] && cached="$(cat "$(ai_cache "$AI_KEY")")"
    local candidate
    for candidate in "$cached" "$(describe "$W_PR_TITLE")" "$(describe "$W_JIRA_SUMMARY")"; do
      if [[ -n "${candidate// /}" ]]; then desc="$candidate"; break; fi
    done
    title="${desc:-$W_LABEL}"
    [[ -n "$W_PR_NUM" ]] && title="#$W_PR_NUM $title"
    [[ -z "$W_TICKET" ]] && ref="$W_BRANCH"
  else
    title="$W_LABEL"
    ref="$W_BRANCH"
  fi
  args+=(--token "title=$title")
  [[ -n "$desc" ]]       && args+=(--token "desc=$desc")              || args+=(--clear-token desc)
  [[ -n "$W_TICKET" ]]   && args+=(--token "ticket=$W_TICKET")        || args+=(--clear-token ticket)
  [[ -n "$W_PARENT" ]]   && args+=(--token "parent=↳ $W_PARENT")      || args+=(--clear-token parent)
  [[ -n "$W_PR_LABEL" ]] && args+=(--token "pr=$W_PR_LABEL")          || args+=(--clear-token pr)
  [[ -n "$W_PR_STATE" ]] && args+=(--token "pr_state=$W_PR_STATE")    || args+=(--clear-token pr_state)
  [[ -n "$ref" ]]        && args+=(--token "ref=$ref")                || args+=(--clear-token ref)
  if "$HERDR" workspace report-metadata "$W_ID" --source "$SOURCE" "${args[@]}" >/dev/null; then
    mkdir -p "$STATE_DIR/last" && touch "$STATE_DIR/last/$W_ID"
    log "$W_ID ${W_BRANCH:-?} title=$title ticket=${W_TICKET:-} parent=${W_PARENT:-} pr=${W_PR_LABEL:-}"
  else
    log "report-metadata failed for $W_ID"
  fi
}

# ---- AI queue: one generator at a time ----

LOCK_DIR=""

take_lock() {
  local lock="$STATE_DIR/ai.lock" pid
  if mkdir "$lock" 2>/dev/null; then
    echo $$ >"$lock/pid"; LOCK_DIR="$lock"; return 0
  fi
  pid="$(cat "$lock/pid" 2>/dev/null)"
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    return 1                                 # a live generator will drain the queue
  fi
  # A pid-less lock may be a generator between mkdir and writing its pid.
  if [[ -z "$pid" ]]; then
    local age
    age="$(file_age_seconds "$lock" 2>/dev/null)" || age=0
    [[ "$age" -ge 5 ]] || return 1
  fi
  log "taking a stale AI lock (pid ${pid:-none})"
  rm -rf "$lock"
  mkdir "$lock" 2>/dev/null || return 1
  echo $$ >"$lock/pid"; LOCK_DIR="$lock"
}

release_lock() {
  [[ -n "$LOCK_DIR" ]] && rm -rf "$LOCK_DIR"
  LOCK_DIR=""
}
trap release_lock EXIT

enqueue() { mkdir -p "$STATE_DIR/queue" && touch "$STATE_DIR/queue/$1"; }

next_queued() {
  local f
  for f in "$STATE_DIR"/queue/*; do
    [[ -e "$f" ]] || return 1
    basename "$f"
    return 0
  done
  return 1
}

# Generates every queued descriptor. Each workspace is collected again right
# before its generation, so a branch switched meanwhile never gets an old
# answer, and two workspaces with the same key share one call.
drain_queue() {
  local id list
  while next_queued >/dev/null; do
    take_lock || return 0
    while id="$(next_queued)"; do
      rm -f "$STATE_DIR/queue/$id"
      list="$("$HERDR" workspace list 2>/dev/null)" || continue
      collect "$list" "$id"
      ai_plan
      if [[ -n "$AI_KEY" && ! -f "$(ai_cache "$AI_KEY")" ]] && ! ai_cooling "$AI_KEY"; then
        # The branch may have changed while the model answered: start over
        # with what the checkout is now instead of publishing the old one.
        if ai_generate && [[ "$(git -C "$W_PATH" branch --show-current 2>/dev/null)" != "$W_BRANCH" ]]; then
          enqueue "$id"
          continue
        fi
      fi
      # Published even without a new descriptor: the workspace was collected
      # afresh, and what was published before may be from another branch.
      publish
    done
    release_lock
    # Something may have been queued between the last check and the release.
  done
}

# ---- sidebar order ----

# Per repo with an out-of-order group: repo_key<TAB>sorted ids<TAB>anchor id.
# Sort key: group (parent ticket, or the ticket itself when there is no parent),
# then ticket, then PR number; ticket keys compare project first, number second.
regroup_plan() {
  jq -r '
    def num: (split("-")[1] // "0") | tonumber? // 0;
    def proj: split("-")[0];
    .result.workspaces as $ws
    | ($ws | map(.workspace_id)) as $order
    | [ $ws[] | select(.worktree != null and .worktree.is_linked_worktree) ]
    | group_by(.worktree.repo_key)[]
    | . as $members
    | ($members[0].worktree.repo_key) as $key
    | ($members | map(.workspace_id)) as $current
    | ($members | sort_by(
        ((.tokens.parent // "") | sub("^↳ "; "")) as $parent
        | (.tokens.ticket // "") as $ticket
        | (if $parent != "" then $parent else $ticket end) as $group
        | [ ($group | proj), ($group | num), ($ticket | proj), ($ticket | num),
            ((.tokens.pr // "") | capture("#(?<n>[0-9]+)")? // {n: "0"} | .n | tonumber), .label ])
      | map(.workspace_id)) as $sorted
    | select($current != $sorted)
    | (first($ws[] | select(.worktree != null and (.worktree.is_linked_worktree | not)
        and .worktree.repo_key == $key) | .workspace_id) // $current[0]) as $lead
    | ($order | index($lead)) as $start
    | (first($order[$start + 1:][] | select(. as $id | $current | index($id) | not)) // "") as $anchor
    | [ $key, ($sorted | join(" ")), $anchor ] | @tsv'
}

regroup() {
  [[ "$REGROUP" == "1" ]] || return 0
  [[ -n "${HERDR_SOCKET_PATH:-}" && -S "$HERDR_SOCKET_PATH" ]] || { log "no herdr socket; skipping regroup"; return 0; }
  command -v nc >/dev/null 2>&1 || { log "nc not installed; skipping regroup"; return 0; }
  local list
  list="$("$HERDR" workspace list 2>/dev/null)" || return 0
  local key sorted anchor request response repo
  while IFS=$'\t' read -r key sorted anchor; do
    [[ -n "$key" ]] || continue
    request="$(jq -cn --arg ids "$sorted" --arg anchor "$anchor" '
      {id: "asmeta:regroup", method: "workspace.move_block",
       params: ({workspace_ids: ($ids | split(" "))}
         + (if $anchor != "" then {before_workspace_id: $anchor} else {} end))}')"
    response="$(printf '%s\n' "$request" | nc -U -w 5 "$HERDR_SOCKET_PATH" 2>/dev/null | head -1)"
    if jq -e '.result' >/dev/null 2>&1 <<<"$response"; then
      repo="${key%/.git}"; log "regrouped ${repo##*/}: $sorted"
    else
      log "move_block failed for $key: ${response:-no response}"
    fi
  done < <(regroup_plan <<<"$list")
}

workspace_from_env() {
  if [[ -n "${HERDR_WORKSPACE_ID:-}" ]]; then
    echo "$HERDR_WORKSPACE_ID"
  elif [[ -n "${HERDR_PLUGIN_EVENT_JSON:-}" ]]; then
    jq -r 'first(.. | objects | .workspace_id? // empty)' <<<"$HERDR_PLUGIN_EVENT_JSON"
  elif [[ -n "${HERDR_PLUGIN_CONTEXT_JSON:-}" ]]; then
    jq -r 'first(.. | objects | .workspace_id? // empty)' <<<"$HERDR_PLUGIN_CONTEXT_JSON"
  fi
}

main() {
  local all=false force=false regen=false ids=()
  for arg in "$@"; do
    case "$arg" in
      --all) all=true ;;
      --force) force=true ;;
      --regen) regen=true ;;
      -*) log "unknown flag $arg"; exit 2 ;;
      *) ids+=("$arg") ;;
    esac
  done
  [[ "${HERDR_PLUGIN_EVENT:-}" == "startup" ]] && all=true
  mkdir -p "$STATE_DIR"
  load_secrets

  local list
  list="$("$HERDR" workspace list 2>/dev/null)" || { log "herdr workspace list failed"; exit 1; }

  if [[ "$all" == true ]]; then
    while IFS= read -r id; do ids+=("$id"); done < <(jq -r '.result.workspaces[].workspace_id' <<<"$list")
  elif [[ ${#ids[@]} -eq 0 ]]; then
    local id
    id="$(workspace_from_env)"
    [[ -n "$id" ]] || { log "no workspace id (pass one or run from a herdr hook)"; exit 2; }
    ids=("$id")
    if [[ "${HERDR_PLUGIN_EVENT:-}" == "workspace.focused" && "$force" == false ]]; then
      local age
      age="$(file_age_seconds "$STATE_DIR/last/$id" 2>/dev/null)" || age=""
      if [[ -n "$age" && "$age" -lt "$FOCUS_THROTTLE_SECONDS" ]]; then
        exit 0
      fi
    fi
  fi

  local id
  for id in "${ids[@]}"; do
    collect "$list" "$id"
    ai_plan
    if [[ -n "$AI_KEY" && "$regen" == true ]]; then
      rm -f "$(ai_cache "$AI_KEY")" "$(ai_cache "$AI_KEY").fail"
    fi
    publish
    if [[ -n "$AI_KEY" && ! -f "$(ai_cache "$AI_KEY")" ]] && ! ai_cooling "$AI_KEY"; then
      enqueue "$id"
    fi
  done
  case "${HERDR_PLUGIN_EVENT:-}" in
    workspace.focused|workspace.renamed) ;;
    *) regroup ;;
  esac
  drain_queue
}

main "$@"
