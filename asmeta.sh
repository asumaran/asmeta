#!/usr/bin/env bash
# asmeta: report the ticket, parent ticket and PR of a workspace branch as
# herdr Space sidebar tokens ($ticket, $parent, $pr).
#
#   asmeta.sh                 workspace from the herdr hook env (event / action)
#   asmeta.sh <workspace_id>  one workspace
#   asmeta.sh --all           every workspace with a checkout
#   --force                   ignore the focus throttle
#
# Sources: branch name (ticket key), `gh pr view` (PR), Jira REST (parent),
# stacks from ~/.claude/asdev.local.md (same file asdev / asgotoissues read).
#
# After reporting, the linked worktrees of each repo are reordered in the
# sidebar by (parent ticket, ticket, PR number) ascending, so subtasks of one
# parent sit together right under the repo row. A worktree without a parent
# groups under its own ticket, so the parent's worktree leads its children.
# ASMETA_REGROUP=0 disables it. Focus events never reorder.
set -uo pipefail

SOURCE="asumaran.asmeta"
HERDR="${HERDR_BIN_PATH:-herdr}"
STATE_DIR="${HERDR_PLUGIN_STATE_DIR:-$HOME/.local/state/asmeta}"
ASDEV_CONFIG="${ASDEV_CONFIG:-$HOME/.claude/asdev.local.md}"
FOCUS_THROTTLE_SECONDS="${ASMETA_FOCUS_THROTTLE_SECONDS:-120}"
REGROUP="${ASMETA_REGROUP:-1}"
TICKET_RE='[A-Z][A-Z0-9]+-[0-9]+'

log() { printf 'asmeta: %s\n' "$*" >&2; }

file_age_seconds() {
  local mtime
  mtime="$(stat -f %m "$1" 2>/dev/null || stat -c %Y "$1" 2>/dev/null)" || return 1
  echo $(( $(date +%s) - mtime ))
}

# Hooks inherit the server environment, which may predate ~/.secrets edits.
load_secrets() {
  # shellcheck disable=SC1090
  [[ -f "$HOME/.secrets" ]] && source "$HOME/.secrets"
  return 0
}

# stack<TAB>base_url<TAB>email<TAB>token_env<TAB>default_project per Jira stack.
jira_stacks() {
  [[ -f "$ASDEV_CONFIG" ]] || return 0
  command -v yq >/dev/null 2>&1 || { log "yq not installed; skipping Jira parents"; return 0; }
  yq -r 'select(di==0) | .stacks | to_entries[]
    | select(.value.jira.base_url != null)
    | [.key, .value.jira.base_url, .value.jira.email, .value.jira.api_token_env, (.value.jira.default_project // "")]
    | @tsv' "$ASDEV_CONFIG" 2>/dev/null
}

# Prints "PARENT_KEY<TAB>PARENT_TYPE" (empty when the ticket has no parent).
# Cached forever in the state dir: a ticket's parent does not change.
jira_parent() {
  local ticket="$1"
  local cache="$STATE_DIR/parents/$ticket"
  if [[ -f "$cache" ]]; then
    cat "$cache"
    return 0
  fi
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
      "$url/rest/api/3/issue/$ticket?fields=parent" 2>/dev/null)"
    case "$status" in
      200)
        local result
        result="$(jq -r '[.fields.parent.key // "", .fields.parent.fields.issuetype.name // ""] | @tsv' "$body")"
        mkdir -p "$STATE_DIR/parents"
        printf '%s\n' "$result" >"$cache"
        rm -f "$body"
        printf '%s\n' "$result"
        return 0
        ;;
      404) continue ;;                       # not in this stack, try the next one
      *) log "jira $name returned $status for $ticket"; continue ;;
    esac
  done <<<"${preferred}${rest}"
  rm -f "$body"
  return 0
}

pr_label() {
  local path="$1" json
  json="$(cd "$path" && gh pr view --json number,state,isDraft 2>/dev/null)" || return 0
  jq -r '"#\(.number)" + (if .isDraft then " draft" elif .state == "MERGED" then " merged" elif .state == "CLOSED" then " closed" else "" end)' <<<"$json"
}

report() {
  local id="$1" path="$2" branch ticket="" pr="" parent="" parent_type=""
  branch="$(git -C "$path" branch --show-current 2>/dev/null)"
  ticket="$(grep -oE "$TICKET_RE" <<<"$branch" | head -1)"
  case "$branch" in
    ""|main|master|develop) ;;
    *) pr="$(pr_label "$path")" ;;
  esac
  if [[ -n "$ticket" ]]; then
    IFS=$'\t' read -r parent parent_type <<<"$(jira_parent "$ticket")"
    # An epic is a container, not a sibling group: only real parents are shown.
    [[ "$parent_type" == "Epic" ]] && parent=""
  fi
  local args=()
  [[ -n "$ticket" ]] && args+=(--token "ticket=$ticket") || args+=(--clear-token ticket)
  [[ -n "$parent" ]] && args+=(--token "parent=↳ $parent") || args+=(--clear-token parent)
  [[ -n "$pr" ]]     && args+=(--token "pr=$pr")         || args+=(--clear-token pr)
  if "$HERDR" workspace report-metadata "$id" --source "$SOURCE" "${args[@]}" >/dev/null; then
    mkdir -p "$STATE_DIR/last" && touch "$STATE_DIR/last/$id"
    log "$id ${branch:-?} ticket=${ticket:-} parent=${parent:-} pr=${pr:-}"
  else
    log "report-metadata failed for $id"
  fi
}

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
  local all=false force=false ids=()
  for arg in "$@"; do
    case "$arg" in
      --all) all=true ;;
      --force) force=true ;;
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
    while IFS= read -r id; do ids+=("$id"); done < <(jq -r '.result.workspaces[] | select(.worktree != null) | .workspace_id' <<<"$list")
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

  local id path
  for id in "${ids[@]}"; do
    path="$(jq -r --arg id "$id" '.result.workspaces[] | select(.workspace_id == $id) | .worktree.checkout_path // empty' <<<"$list")"
    if [[ -z "$path" || ! -d "$path" ]]; then
      "$HERDR" workspace report-metadata "$id" --source "$SOURCE" \
        --clear-token ticket --clear-token parent --clear-token pr >/dev/null 2>&1
      continue
    fi
    report "$id" "$path"
  done
  [[ "${HERDR_PLUGIN_EVENT:-}" == "workspace.focused" ]] || regroup
}

main "$@"
