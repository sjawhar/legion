#!/usr/bin/env bash
# The acceptance observer for LEGION-1986's Task 6.4, the operator's live multi-repository run.
# It never starts a daemon, Dispatch, or any agent of its own, and it writes nothing remote: every
# admission, push, pull request and Dispatch write in the spec's acceptance checklist is the
# operator's and the agents' own.
#
# Every judgment reads a structured source: the daemon's state JSON (GET /legion/v1/state),
# Dispatch's own JSON API (GET /api/v1/issues/<key> and /events, the same routes the dashboard
# reads — not `dispatch read`'s human prose, which truncates and has no stable grammar), GitHub's
# API (`gh api ... --jq`), and `kubectl get`/`logs`/`exec` as the operator, with `exec` run as the
# role itself (below), never as the bare pod shell. The one exception is a daemon message whose
# exact format the daemon itself produces (quoted below with its file:line): those are matched as
# the complete literal sentence, bound to the daemon's own actor identity, with the repository
# names the run supplies substituted in, never a loose substring over any author's messages.
#
# Usage:
#   bash scripts/e2e/multi-repo-acceptance.sh record-head <T>
#   bash scripts/e2e/multi-repo-acceptance.sh children <parent KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh branch-base <T child KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh pin <C> <T merge sha>
#   bash scripts/e2e/multi-repo-acceptance.sh done <KEY>...
#   bash scripts/e2e/multi-repo-acceptance.sh second-owner <O child KEY> <pod> [container]
#   bash scripts/e2e/multi-repo-acceptance.sh cache <issue KEY> <pod> [container]
#   bash scripts/e2e/multi-repo-acceptance.sh negative pr-in-x before <T child KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh negative pr-in-x after <run id> <T child KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh negative no-repo <KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh negative repo-edit before <child KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh negative repo-edit after <run id> <child KEY>
#
# `negative pr-in-x` and `negative repo-edit` are explicit two-call protocols. `before` mints a
# fresh run id, captures a baseline exclusively under its own per-run directory (never shared with
# another run, so two overlapping captures can never overwrite each other), and prints
# `BASELINE <name>: captured` — never PASS, since capturing a baseline is not an acceptance
# judgment. `after` takes that exact run id back (validated: no `/`, no `..`), refuses a missing,
# foreign (another issue, operation, project, daemon, or a generation that moved since the
# baseline), or stale (more than 24 hours old) baseline, and PASSes only once GitHub's own
# pull-request-number cursor or Dispatch's own event sequence — never a wall-clock comparison,
# which clock skew between the operator, Dispatch and GitHub could fool — shows the negative
# control's operation demonstrably happened after the baseline, and that the daemon's own side was
# unaffected by it.
#
# Required inputs (every one an operator input; the script refuses to start without any of
# them, naming the first missing one, and never carries a literal of its own):
#   LEGION_E2E_REPO_T       the toolchain repository, owner/name
#   LEGION_E2E_REPO_C       the consumer repository, owner/name
#   LEGION_E2E_PIN_FILE     C's pin file path (the path inside C that names T's pinned commit)
#   LEGION_E2E_REPO_X       the negative control's repository, owner/name
#   LEGION_E2E_REPO_O       the second owner's repository, owner/name
#   LEGION_E2E_DAEMON_URL   the daemon's API root (GET /legion/v1/state, GET /healthz)
#   DISPATCH_URL            Dispatch's own API root (GET /api/v1/issues/<key>[/events])
#   DISPATCH_TOKEN_FILE     the Dispatch bearer token file
#   MULTIREPO_EVIDENCE_DIR  where each observation's evidence file is kept
set -Eeuo pipefail

self=${0##*/}

fail() {
  echo "$self: $1" >&2
  exit 1
}

usage() {
  echo "usage: multi-repo-acceptance.sh <observation> [args...]" >&2
  echo "  record-head <T>" >&2
  echo "  children <parent KEY>" >&2
  echo "  branch-base <T child KEY>" >&2
  echo "  pin <C> <T merge sha>" >&2
  echo "  done <KEY>..." >&2
  echo "  second-owner <O child KEY> <pod> [container]" >&2
  echo "  cache <issue KEY> <pod> [container]" >&2
  echo "  negative pr-in-x before <T child KEY>" >&2
  echo "  negative pr-in-x after <run id> <T child KEY>" >&2
  echo "  negative no-repo <KEY>" >&2
  echo "  negative repo-edit before <child KEY>" >&2
  echo "  negative repo-edit after <run id> <child KEY>" >&2
  exit 2
}

# validate_argv checks $1.. names a known observation with the right shape, before anything reads
# an environment variable, a tool, or the network: an absent or malformed observation is a usage
# error, not a dependency failure.
validate_argv() {
  [ "$#" -ge 1 ] || usage
  case "$1" in
    record-head | children | branch-base) [ "$#" = 2 ] || usage ;;
    pin) [ "$#" = 3 ] || usage ;;
    done) [ "$#" -ge 2 ] || usage ;;
    second-owner) { [ "$#" = 3 ] || [ "$#" = 4 ]; } || usage ;;
    cache) { [ "$#" = 3 ] || [ "$#" = 4 ]; } || usage ;;
    negative)
      [ "$#" -ge 2 ] || usage
      case "$2" in
        pr-in-x | repo-edit)
          case "${3:-}" in
            before) [ "$#" = 4 ] || usage ;;
            after) [ "$#" = 5 ] || usage ;;
            *) usage ;;
          esac
          ;;
        no-repo) [ "$#" = 3 ] || usage ;;
        *) usage ;;
      esac
      ;;
    *) usage ;;
  esac
}
validate_argv "$@"

require_env() {
  local name=$1 what=$2
  [ -n "${!name:-}" ] || fail "$name is unset: $what (an operator input, never a literal here)"
}

require_env LEGION_E2E_REPO_T "the toolchain repository, owner/name"
require_env LEGION_E2E_REPO_C "the consumer repository, owner/name"
require_env LEGION_E2E_PIN_FILE "the path inside the consumer repository that names the toolchain's pinned commit"
require_env LEGION_E2E_REPO_X "the negative control's repository, owner/name"
require_env LEGION_E2E_REPO_O "the second owner's repository, owner/name"
require_env LEGION_E2E_DAEMON_URL "the daemon's API root"
require_env DISPATCH_URL "Dispatch's own API root"
require_env DISPATCH_TOKEN_FILE "the Dispatch bearer token file"
require_env MULTIREPO_EVIDENCE_DIR "where this run's observation evidence is kept"

# repo_shape_re is a GitHub owner/name pair, conservatively: letters, digits, dot, dash,
# underscore, exactly one slash. Nothing stricter than GitHub's own rules, but enough to reject
# shell metacharacters and a bare owner or name before the value reaches gh, jq, or a URL.
repo_shape_re='^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$'
for name in LEGION_E2E_REPO_T LEGION_E2E_REPO_C LEGION_E2E_REPO_X LEGION_E2E_REPO_O; do
  [[ ${!name} =~ $repo_shape_re ]] || fail "$name is $(printf '%q' "${!name}"), not an owner/name repository"
done

# lower lowercases its argument (ASCII): GitHub owner and repository names are case-insensitive,
# so every comparison below folds case before comparing, never after.
lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

repo_t=$(lower "$LEGION_E2E_REPO_T") repo_c=$(lower "$LEGION_E2E_REPO_C")
repo_x=$(lower "$LEGION_E2E_REPO_X") repo_o=$(lower "$LEGION_E2E_REPO_O")

[ "$repo_t" != "$repo_c" ] || fail "LEGION_E2E_REPO_T and LEGION_E2E_REPO_C both name $LEGION_E2E_REPO_T (case-insensitively): the observations need distinct repositories"
[ "$repo_t" != "$repo_x" ] || fail "LEGION_E2E_REPO_T and LEGION_E2E_REPO_X both name $LEGION_E2E_REPO_T (case-insensitively): the observations need distinct repositories"
[ "$repo_t" != "$repo_o" ] || fail "LEGION_E2E_REPO_T and LEGION_E2E_REPO_O both name $LEGION_E2E_REPO_T (case-insensitively): the observations need distinct repositories"
[ "$repo_c" != "$repo_x" ] || fail "LEGION_E2E_REPO_C and LEGION_E2E_REPO_X both name $LEGION_E2E_REPO_C (case-insensitively): the observations need distinct repositories"
[ "$repo_c" != "$repo_o" ] || fail "LEGION_E2E_REPO_C and LEGION_E2E_REPO_O both name $LEGION_E2E_REPO_C (case-insensitively): the observations need distinct repositories"
[ "$repo_x" != "$repo_o" ] || fail "LEGION_E2E_REPO_X and LEGION_E2E_REPO_O both name $LEGION_E2E_REPO_X (case-insensitively): the observations need distinct repositories"

t_owner=$(lower "${LEGION_E2E_REPO_T%%/*}")
o_owner=$(lower "${LEGION_E2E_REPO_O%%/*}")
[ "$t_owner" != "$o_owner" ] || fail "LEGION_E2E_REPO_T and LEGION_E2E_REPO_O share an owner ($t_owner, case-insensitively): the second-owner observation needs two owners"

for tool in curl jq gh; do
  command -v "$tool" >/dev/null || fail "$tool is required and is not on PATH"
done

mkdir -p "$MULTIREPO_EVIDENCE_DIR"

# The health probe's own temporary file is allocated under the evidence directory (operator-owned,
# created just above) with mktemp, never a predictable /tmp path: mktemp refuses to create over an
# existing path (including a symlink), so a pre-placed link at a guessed name cannot redirect the
# write.
healthz_file=$(mktemp "$MULTIREPO_EVIDENCE_DIR/healthz.XXXXXX")
healthz_status=$(curl -sS -o "$healthz_file" -w '%{http_code}' --max-time 15 "$LEGION_E2E_DAEMON_URL/healthz" 2>/dev/null) || healthz_status=000
healthz_body=$(cat "$healthz_file" 2>/dev/null || true)
rm -f "$healthz_file"
[ "$healthz_status" = 200 ] && [ "$healthz_body" = '{"status":"ok"}' ] ||
  fail "LEGION_E2E_DAEMON_URL $LEGION_E2E_DAEMON_URL is not a Legion daemon: GET /healthz answered $healthz_status $healthz_body"

dispatch_token=$(cat "$DISPATCH_TOKEN_FILE") || fail "could not read DISPATCH_TOKEN_FILE"
[ -n "$dispatch_token" ] || fail "DISPATCH_TOKEN_FILE is empty"

# raw_fetch <evidence file> <command...> runs command with its stdout redirected straight to a
# fresh, uniquely-named temporary file (byte for byte, trailing bytes included — never through
# $(...), which strips trailing newlines and is why an earlier round's pin evidence was two bytes
# short), then renames it onto <evidence file> — a same-filesystem rename is atomic, so a second
# invocation of this script against the same evidence directory at the same time (two concurrent
# `before` calls, say) never leaves a reader mid-read of a truncated or interleaved file: every
# reader sees either the whole old file or the whole new one. A nonzero exit fails the whole
# observation; the caller then reads <evidence file> itself, with jq's own filename argument
# (never a second `cat`/`$(...)` round trip) when it needs to parse it.
raw_fetch() {
  local file=$1
  shift
  local tmp
  tmp=$(mktemp "$file.XXXXXX") || fail "could not create a temporary file for $file"
  if ! "$@" >"$tmp"; then
    rm -f "$tmp"
    fail "$*: failed"
  fi
  mv -f "$tmp" "$file"
}

# state_raw refreshes $MULTIREPO_EVIDENCE_DIR/state.raw from GET /legion/v1/state (no bearer: the
# route is open, the same bytes `legion state --json` prints) and prints its path.
state_raw() {
  local file="$MULTIREPO_EVIDENCE_DIR/state.raw"
  raw_fetch "$file" curl -sS --fail --max-time 30 "$LEGION_E2E_DAEMON_URL/legion/v1/state"
  printf '%s' "$file"
}

# dispatch_issue_raw <KEY> refreshes $MULTIREPO_EVIDENCE_DIR/dispatch-issue-<KEY>.raw from
# Dispatch's own GET /api/v1/issues/<KEY> (the same JSON the dashboard reads, carrying .repo,
# .resolved_repo, .status, .generation and .children[] structurally — never `dispatch read`'s
# human prose) and prints its path.
dispatch_issue_raw() {
  local key=$1
  local file="$MULTIREPO_EVIDENCE_DIR/dispatch-issue-$key.raw"
  raw_fetch "$file" curl -sS --fail --max-time 30 -H "Authorization: Bearer $dispatch_token" "$DISPATCH_URL/api/v1/issues/$key"
  printf '%s' "$file"
}

# dispatch_events_raw <KEY> refreshes $MULTIREPO_EVIDENCE_DIR/dispatch-events-<KEY>.raw from
# Dispatch's own GET /api/v1/issues/<KEY>/events (newest first, up to 200): each event's .type,
# .seq, .actor and, for message.created, the complete .payload.body (never truncated, unlike
# `dispatch read`'s event summaries). Prints its path.
dispatch_events_raw() {
  local key=$1
  local file="$MULTIREPO_EVIDENCE_DIR/dispatch-events-$key.raw"
  raw_fetch "$file" curl -sS --fail --max-time 30 -H "Authorization: Bearer $dispatch_token" \
    "$DISPATCH_URL/api/v1/issues/$key/events?order=desc&limit=200"
  printf '%s' "$file"
}

# issue_project <KEY> is the Dispatch project prefix of an issue key (DaemonSession's own
# derivation, internal/dispatch/client.go:287-289: the text before the first '-'), used both to
# bind a baseline to its project and to name the daemon's own actor id, legion-daemon:<PROJECT>.
issue_project() { printf '%s' "${1%%-*}"; }

# summary <name> <text> writes an observation's own computed summary to its evidence file
# (<name>.txt) and prints it back — the fields it judged and what it found, never the sole record
# of a structured read: every structured source this script reads also keeps its own
# <source>.raw file beside it (state.raw, dispatch-issue-<key>.raw, dispatch-events-<key>.raw),
# written byte for byte by raw_fetch.
summary() {
  local name=$1 text=$2
  printf '%s' "$text" >"$MULTIREPO_EVIDENCE_DIR/$name.txt"
  printf '%s' "$text"
}

check() {
  local name=$1 ok=$2
  if [ "$ok" = 1 ]; then
    printf 'CHECK %s: PASS\n' "$name"
  else
    printf 'CHECK %s: FAIL\n' "$name" >&2
    exit 1
  fi
}

# sha_re matches a full Git commit object id (40 lowercase hex digits): what every comparison
# below that claims to have a commit requires, so an empty or truncated value never passes as one.
sha_re='^[0-9a-f]{40}$'

# default_branch <owner/repo> is GitHub's own default branch name for it, required nonempty.
default_branch() {
  local repo=$1 branch
  branch=$(gh api "repos/$repo" --jq .default_branch) || fail "gh api repos/$repo failed"
  [ -n "$branch" ] || fail "repos/$repo reports an empty default_branch"
  printf '%s' "$branch"
}

# record-head <T>: the commit at T's default branch head, read now (run when T's child is
# recorded), stored for branch-base to compare the child branch's first commit's parent against.
record_head() {
  local repo=$1 default sha ok=1
  [ "$(lower "$repo")" = "$repo_t" ] || fail "record-head's repository argument $repo is not LEGION_E2E_REPO_T ($LEGION_E2E_REPO_T)"
  default=$(default_branch "$repo")
  sha=$(gh api "repos/$repo/branches/$default" --jq .commit.sha) || fail "gh api repos/$repo/branches/$default failed"
  [[ $sha =~ $sha_re ]] || { ok=0; sha=""; }
  summary record-head "repo: $repo
default branch: $default
head: $sha
" >/dev/null
  echo "$sha"
  check record-head "$ok"
}

# children <parent KEY>: one child resolves to T and one to C, structurally, in both Dispatch's own
# record (.resolved_repo) and the daemon's state (.issues[<key>].repo).
children() {
  local parent=$1 keys key d_repo s_repo saw_t=0 saw_c=0 mismatch=0 out=""
  keys=$(jq -r '.children[].key' "$(dispatch_issue_raw "$parent")")
  [ -n "$keys" ] || fail "children: $parent has no children on Dispatch"
  while IFS= read -r key; do
    [ -n "$key" ] || continue
    d_repo=$(jq -r '.resolved_repo // "none"' "$(dispatch_issue_raw "$key")")
    s_repo=$(jq -r --arg k "$key" '.issues[$k].repo // "none"' "$(state_raw)")
    out+="$key: dispatch=$d_repo state=$s_repo
"
    [ "$(lower "$d_repo")" = "$(lower "$s_repo")" ] || mismatch=1
    [ "$(lower "$d_repo")" != "$repo_t" ] || saw_t=1
    [ "$(lower "$d_repo")" != "$repo_c" ] || saw_c=1
  done <<<"$keys"
  summary children "$out" >/dev/null
  printf '%s' "$out"
  check children "$([ "$mismatch" = 0 ] && [ "$saw_t" = 1 ] && [ "$saw_c" = 1 ] && echo 1 || echo 0)"
}

# branch-base <T child KEY>: legion/<child>'s first commit's first parent on GitHub equals the
# stored record-head, exactly (both required to be full commit ids).
branch_base() {
  local child=$1 default stored_head first_commit parent_sha ok=0
  [ -s "$MULTIREPO_EVIDENCE_DIR/record-head.txt" ] || fail "branch-base: run record-head first"
  stored_head=$(grep '^head: ' "$MULTIREPO_EVIDENCE_DIR/record-head.txt" | cut -d' ' -f2)
  [[ $stored_head =~ $sha_re ]] || fail "branch-base: the stored record-head is not a full commit id ($stored_head); record-head must have failed"
  default=$(default_branch "$LEGION_E2E_REPO_T")
  first_commit=$(gh api "repos/$LEGION_E2E_REPO_T/compare/$default...legion/$child" --jq '.commits[0].sha') ||
    fail "gh api repos/$LEGION_E2E_REPO_T/compare/$default...legion/$child failed"
  [[ $first_commit =~ $sha_re ]] || fail "branch-base: legion/$child's first commit on top of $default is not a full commit id ($first_commit)"
  parent_sha=$(gh api "repos/$LEGION_E2E_REPO_T/commits/$first_commit" --jq '.parents[0].sha') ||
    fail "gh api repos/$LEGION_E2E_REPO_T/commits/$first_commit failed"
  [[ $parent_sha =~ $sha_re ]] && [ "$parent_sha" = "$stored_head" ] && ok=1
  summary branch-base "child: $child
first commit: $first_commit
first parent: $parent_sha
record-head: $stored_head
" >/dev/null
  check branch-base "$ok"
}

# pin <C> <T merge sha>: C's pin file, on C's default branch, names the merge sha exactly; both the
# repository and the sha argument are validated before anything is fetched. The file's own bytes go
# straight to evidence/pin.txt by redirection, never through a $(...) capture that would strip its
# trailing bytes.
pin() {
  local repo=$1 sha=$2 default content_file="$MULTIREPO_EVIDENCE_DIR/pin.txt" ok=0
  [ "$(lower "$repo")" = "$repo_c" ] || fail "pin's repository argument $repo is not LEGION_E2E_REPO_C ($LEGION_E2E_REPO_C)"
  [[ $sha =~ $sha_re ]] || fail "pin's merge-sha argument $(printf '%q' "$sha") is not a full 40-character commit id"
  default=$(default_branch "$repo")
  raw_fetch "$content_file.b64" gh api "repos/$repo/contents/$LEGION_E2E_PIN_FILE?ref=$default" --jq .content
  base64 -d <"$content_file.b64" >"$content_file" || fail "pin: $LEGION_E2E_PIN_FILE's content is not valid base64"
  rm -f "$content_file.b64"
  [ -s "$content_file" ] || fail "pin: $LEGION_E2E_PIN_FILE on $repo@$default is empty"
  cat "$content_file"
  grep -qF "$sha" "$content_file" && ok=1
  check pin "$ok"
}

# done <KEY>...: every named issue's Dispatch status (.status, structurally) is done.
done_check() {
  local key out="" ok=1 status
  [ "$#" -gt 0 ] || fail "done: name at least one issue key"
  for key in "$@"; do
    status=$(jq -r '.status' "$(dispatch_issue_raw "$key")")
    if [ "$status" = "done" ]; then
      out+="$key: done
"
    else
      out+="$key: $status
"
      ok=0
    fi
  done
  summary "done" "$out" >/dev/null
  printf '%s' "$out"
  check "done" "$ok"
}

# pod_issue_label <pod> is the pod's legion.dev/issue label, or a failure naming why it could not
# be read.
pod_issue_label() {
  local pod=$1
  kubectl get pod "$pod" -o 'jsonpath={.metadata.labels.legion\.dev/issue}' 2>/dev/null ||
    fail "kubectl get pod $pod failed: cannot confirm whose pod it is"
}

# require_pod_for_issue <pod> <issue> refuses when pod's legion.dev/issue label does not name
# issue: an operator's pod argument always names the pod, never implies it.
require_pod_for_issue() {
  local pod=$1 issue=$2 label
  label=$(pod_issue_label "$pod")
  [ "$label" = "$issue" ] || fail "pod $pod's legion.dev/issue label is $(printf '%q' "$label"), not $issue: this is not that issue's pod"
}

# ---- running as the role, not the bare pod shell (N1) ----------------------------------------
#
# `kubectl exec` carries none of a role's own variables — only PATH, HOME and POD_UID, the three a
# container spec sets on exec itself (packages/daemon/internal/runtime/sandbox/manifest.go,
# podkind.go); LEGION_GH_OWNERS_DIR, GH_CONFIG_DIR, LEGION_GH_PATH and LEGION_WORKSPACE are plain
# variables the launcher's start command gave the role's own process, never exec's. `legion gh`
# (found via a bare exec's PATH) refuses with exit 2 when LEGION_GH_PATH is unset
# (cmd/legion/gh.go), so a read run as the bare pod can never pass. Stage 4a's own live proof runs
# these exact probes as the role instead, reading its environment from /proc/<pid>/environ and
# re-executing under exactly that environment with `env -i`
# (internal/runtime/sandbox/live_lifecycle_test.go:1096-1105, asAgent).

# as_role_find_and_run is the POSIX-sh fragment a single `kubectl exec ... -- sh -c` sends into
# the pod: find the one process in this container whose environment carries LEGION_GH_PATH (the
# role's own, however it is named or spawned — never a hardcoded process name), then re-exec the
# script this function's own first argument carries under exactly that environment, exactly as
# asAgent's xargs/env -i pattern does. Single-quoted deliberately: every $-expansion inside runs
# in the pod's own remote shell, never this one.
# shellcheck disable=SC2016
as_role_find_and_run='
pid=""
for d in /proc/[0-9]*; do
  [ -r "$d/environ" ] || continue
  if tr "\0" "\n" <"$d/environ" 2>/dev/null | grep -q "^LEGION_GH_PATH="; then
    pid=${d#/proc/}
    break
  fi
done
if [ -z "$pid" ]; then
  echo "as_role: no process in this container carries LEGION_GH_PATH" >&2
  exit 1
fi
xargs -0 sh -c '"'"'inner=$1; shift; exec env -i "$@" sh -c "$inner"'"'"' agent-env "$1" <"/proc/$pid/environ"
'

# as_role <pod> <container> <script> runs script with sh -c under the role's own environment, as
# the role's own bash tool would run it — never the bare pod shell's.
as_role() {
  local pod=$1 container=$2 script=$3
  kubectl exec "$pod" -c "$container" -- sh -c "$as_role_find_and_run" as-role "$script"
}

# second-owner <O child KEY> <pod> [container]: its pull request is in O (structurally,
# .pullRequest.repo) and GitHub confirms it merged there (gh api .../pulls/<n> --jq .merged — the
# daemon's own `done` phase is accepted only alongside `production_check`, never alone, since the
# phase is not merge proof by itself, linger.go:32-33); the pod is bound to the issue by its
# legion.dev/issue label, as `cache` binds it. The credential evidence runs live, as the role (N1):
# its own LEGION_GH_OWNERS_DIR and GH_CONFIG_DIR are read and checked against the expected
# directories first, then a read of T (`gh api repos/<T> --jq .full_name`, must equal T exactly), a
# positive control (a dry-run push to O's own repository, which must succeed — proving the
# network, the credential helper and the ref are all sound before the negative probe can be
# trusted), and the negative probe itself: a dry-run push to T, which must fail with a permission
# refusal (403, or "Permission to ... denied") — never merely "any nonzero exit", which a bad ref,
# an unreachable network or an unanswered credential helper would also give (N2). Both pushes run
# `--no-verify -c core.hooksPath=/dev/null` (a dry-run still runs the workspace's own pre-push
# hook, which is agent-writable) from the issue's own clone's git dir ($LEGION_WORKSPACE/.git).
second_owner() {
  local child=$1 pod=$2 container=${3:-implementer} pr_repo pr_number phase merged ok=1
  command -v kubectl >/dev/null || fail "second-owner: kubectl is required and is not on PATH"
  require_pod_for_issue "$pod" "$child"

  local state_file
  state_file=$(state_raw)
  pr_repo=$(jq -r --arg k "$child" '.issues[$k].pullRequest.repo // "none"' "$state_file")
  pr_number=$(jq -r --arg k "$child" '.issues[$k].pullRequest.number // empty' "$state_file")
  phase=$(jq -r --arg k "$child" '.issues[$k].phase // "none"' "$state_file")
  [ "$(lower "$pr_repo")" = "$repo_o" ] || ok=0
  case "$phase" in
    production_check | done) ;;
    *) ok=0 ;;
  esac
  merged=false
  if [ -n "$pr_number" ]; then
    merged=$(gh api "repos/$LEGION_E2E_REPO_O/pulls/$pr_number" --jq .merged) || merged=false
  fi
  [ "$merged" = true ] || ok=0

  local env_check owners_dir config_dir want_config_dir
  # shellcheck disable=SC2016 # expands in the pod's own remote shell, as a role's bash tool would
  env_check=$(as_role "$pod" "$container" 'printf "%s|%s" "$LEGION_GH_OWNERS_DIR" "$GH_CONFIG_DIR"' 2>/dev/null) ||
    fail "second-owner: could not read the role's own environment in $pod ($container)"
  owners_dir=${env_check%%|*}
  config_dir=${env_check#*|}
  want_config_dir="$owners_dir/$o_owner"
  [ -n "$owners_dir" ] && [ "$config_dir" = "$want_config_dir" ] || ok=0

  local read_full_name
  read_full_name=$(as_role "$pod" "$container" "gh api repos/$LEGION_E2E_REPO_T --jq .full_name" 2>/dev/null) || read_full_name=""
  [ "$(lower "$read_full_name")" = "$repo_t" ] || ok=0

  local probe_branch push_flags
  probe_branch="legion-probe-$$-$(date +%s)"
  push_flags='-c core.hooksPath=/dev/null push --no-verify --dry-run'

  local push_o_out push_o_status=0
  push_o_out=$(as_role "$pod" "$container" \
    "git --git-dir=\"\$LEGION_WORKSPACE/.git\" $push_flags \"https://github.com/$LEGION_E2E_REPO_O\" \"HEAD:refs/heads/$probe_branch\"" 2>&1) || push_o_status=$?
  [ "$push_o_status" = 0 ] || ok=0

  local push_t_out push_t_status=0
  push_t_out=$(as_role "$pod" "$container" \
    "git --git-dir=\"\$LEGION_WORKSPACE/.git\" $push_flags \"https://github.com/$LEGION_E2E_REPO_T\" \"HEAD:refs/heads/$probe_branch\"" 2>&1) || push_t_status=$?
  [ "$push_t_status" != 0 ] || ok=0
  printf '%s' "$push_t_out" | grep -qiE '403|Permission to .* denied' || ok=0

  summary second-owner "pullRequest.repo: $pr_repo
pullRequest.number: $pr_number
phase: $phase
merged (GitHub, repos/$LEGION_E2E_REPO_O/pulls/$pr_number): $merged
role env: LEGION_GH_OWNERS_DIR=$owners_dir GH_CONFIG_DIR=$config_dir (want $want_config_dir)
as role: gh api repos/$LEGION_E2E_REPO_T --jq .full_name = $read_full_name
positive control: dry-run push to $LEGION_E2E_REPO_O as role, exit $push_o_status
---
$push_o_out
---
negative probe: dry-run push to $LEGION_E2E_REPO_T as role, exit $push_t_status
---
$push_t_out
" >/dev/null
  check second-owner "$ok"
}

# cache <issue KEY> <pod> [container]: the named pod's legion.dev/issue label matches the issue,
# its fetch container's log carries the producer's exact, anchored cache-stream-with-delta success
# line (packages/daemon/cmd/legion/workspace_fetch.go:83, the form workspace_fetch_test.go:572
# pins), and the daemon's state shows that issue's repository's cache ready.
cache_success_re='^workspace-init fetch: [0-9,]+ bytes from the repository cache .+ in [0-9.]+(µs|ms|s), its history verified in [0-9.]+(µs|ms|s); [0-9,]+ bytes from GitHub for the delta \([0-9]+ refs?\)$'
cache_check() {
  local key=$1 pod=$2 container=${3:-workspace-fetch} repo ready logs_file ok=1
  command -v kubectl >/dev/null || fail "cache: kubectl is required and is not on PATH"
  require_pod_for_issue "$pod" "$key"
  logs_file="$MULTIREPO_EVIDENCE_DIR/cache-logs-$key.raw"
  raw_fetch "$logs_file" kubectl logs "$pod" -c "$container"
  grep -qE "$cache_success_re" "$logs_file" || ok=0
  local state_file
  state_file=$(state_raw)
  repo=$(jq -r --arg k "$key" '.issues[$k].repo // "none"' "$state_file")
  [ "$repo" != "none" ] || ok=0
  ready=$(jq -r --arg r "$repo" '.repositories.mapped[] | select((.repo|ascii_downcase)==($r|ascii_downcase)) | .cache.ready' "$state_file")
  [ "$ready" = true ] || ok=0
  summary cache "issue: $key
pod: $pod ($container)
repo: $repo
cache.ready: $ready
---
$(cat "$logs_file")
" >/dev/null
  check cache "$ok"
}

# ---- the two-call baseline protocol (negative pr-in-x, negative repo-edit) ------------------

# run_id_re is what a run id may look like: safe to use as one path component. It rejects a `/`
# outright and a leading `.` (so `.` and `..` are both refused as whole components, and `..`
# cannot appear as a path-traversal substring of a longer id either, since every character after
# the first must still match this class and the dedicated `..` check below catches it there).
run_id_re='^[A-Za-z0-9][A-Za-z0-9._-]*$'
validate_run_id() {
  local run_id=$1
  [[ $run_id =~ $run_id_re ]] || fail "run id $(printf '%q' "$run_id") is not a bare id (letters, digits, dot, dash, underscore, no leading dot)"
  case "$run_id" in
    *..*) fail "run id $(printf '%q' "$run_id") contains '..'" ;;
  esac
}

# baseline_dir <run id> is the per-run directory a baseline lives under: unique per invocation of
# `before`, so two overlapping captures can never collide or overwrite each other. run_id is
# validated by every caller before this is used in a path.
baseline_dir() {
  printf '%s/runs/%s' "$MULTIREPO_EVIDENCE_DIR" "$1"
}

# baseline_capture <operation> <issue> <extra JSON object> mints a fresh run id, creates its
# directory and its baseline file exclusively (mkdir, then `set -o noclobber`, both of which fail
# loudly rather than overwrite), and prints the run id and a BASELINE line — never PASS: capturing
# a baseline observes nothing about the negative control yet. The baseline is bound to the
# project (the issue key's own prefix) and the issue's current generation (extra's own
# `.generation`, when it carries one), so a hand-over or re-admission between `before` and `after`
# is caught as a foreign baseline rather than compared across two different issue lifetimes.
baseline_capture() {
  local operation=$1 issue=$2 extra=$3
  local run_id project generation dir started_at payload
  run_id="${operation}-${issue}-$(date -u +%Y%m%dT%H%M%SZ)-$$-$RANDOM"
  project=$(issue_project "$issue")
  generation=$(jq -r '.generation // -1' <<<"$extra")
  dir=$(baseline_dir "$run_id")
  mkdir -p "$MULTIREPO_EVIDENCE_DIR/runs"
  mkdir "$dir" 2>/dev/null || fail "a baseline directory for run $run_id already exists; this should be impossible for a freshly minted run id"
  started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  payload=$(jq -n --arg run "$run_id" --arg op "$operation" --arg issue "$issue" --arg project "$project" \
    --arg daemon "$LEGION_E2E_DAEMON_URL" --arg started "$started_at" --argjson generation "$generation" --argjson extra "$extra" \
    '{run_id: $run, operation: $op, issue: $issue, project: $project, daemon_url: $daemon, started_at: $started, generation: $generation} + $extra')
  (
    set -o noclobber
    printf '%s' "$payload" >"$dir/baseline.json"
  ) 2>/dev/null || fail "the baseline file for run $run_id already exists; this should be impossible for a freshly minted run id"
  printf '%s' "$payload" >"$MULTIREPO_EVIDENCE_DIR/negative-$operation-$issue-$run_id.txt"
  echo "RUN_ID: $run_id"
  echo "BASELINE $operation-$issue: captured"
}

# baseline_verify <operation> <issue> <run id> reads and validates the baseline: a safe id, present,
# not foreign (operation, issue, project and daemon URL all match this call's, and the issue's
# current generation still equals the baseline's — a hand-over or re-admission since `before`
# invalidates the comparison), and not older than 24 hours (a negative control's operator action is
# expected to complete well within one working day; a much older file is a stale leftover from
# another session, never evidence for this one). Prints the baseline JSON on success.
baseline_verify() {
  local operation=$1 issue=$2 run_id=$3
  validate_run_id "$run_id"
  local dir
  dir=$(baseline_dir "$run_id")
  [ -f "$dir/baseline.json" ] || fail "no baseline for run $run_id: run 'negative $operation before $issue' first"
  local baseline
  baseline=$(cat "$dir/baseline.json")
  local b_op b_issue b_daemon b_started b_project b_generation
  b_op=$(jq -r '.operation' <<<"$baseline")
  b_issue=$(jq -r '.issue' <<<"$baseline")
  b_daemon=$(jq -r '.daemon_url' <<<"$baseline")
  b_started=$(jq -r '.started_at' <<<"$baseline")
  b_project=$(jq -r '.project' <<<"$baseline")
  b_generation=$(jq -r '.generation' <<<"$baseline")
  [ "$b_op" = "$operation" ] || fail "run $run_id's baseline is for operation $b_op, not $operation: a foreign baseline"
  [ "$b_issue" = "$issue" ] || fail "run $run_id's baseline is for issue $b_issue, not $issue: a foreign baseline"
  [ "$b_project" = "$(issue_project "$issue")" ] || fail "run $run_id's baseline is for project $b_project, not $(issue_project "$issue"): a foreign baseline"
  [ "$b_daemon" = "$LEGION_E2E_DAEMON_URL" ] || fail "run $run_id's baseline is against $b_daemon, not $LEGION_E2E_DAEMON_URL: a foreign baseline"
  local started_epoch now_epoch
  started_epoch=$(date -u -d "$b_started" +%s 2>/dev/null) || fail "run $run_id's baseline has an unreadable started_at ($b_started)"
  now_epoch=$(date -u +%s)
  [ $((now_epoch - started_epoch)) -le 86400 ] || fail "run $run_id's baseline is more than 24h old (captured $b_started): too stale to trust; capture a fresh one with 'negative $operation before $issue'"
  local now_generation
  now_generation=$(jq -r --arg k "$issue" '.issues[$k].generation // -1' "$(state_raw)")
  [ "$now_generation" = "$b_generation" ] || fail "run $run_id's baseline was taken at generation $b_generation; $issue is now generation $now_generation (a hand-over or re-admission since): capture a fresh baseline"
  printf '%s' "$baseline"
}

# negative_issue_repo_of <control> <issue> requires issue already recorded on T or C, in the
# daemon's own state (.issues[<key>].repo), and returns it: the control needs a real recorded
# child to mean anything.
negative_issue_repo_of() {
  local control=$1 issue=$2 repo
  repo=$(jq -r --arg k "$issue" '.issues[$k].repo // "none"' "$(state_raw)")
  { [ "$(lower "$repo")" = "$repo_t" ] || [ "$(lower "$repo")" = "$repo_c" ]; } ||
    fail "negative $control: $issue's recorded repository is $repo, neither T ($LEGION_E2E_REPO_T) nor C ($LEGION_E2E_REPO_C): not the recorded child this control needs"
  printf '%s' "$repo"
}

# negative pr-in-x before|after <run id>? <T child KEY>: `before` captures the issue's daemon
# record (repo, pullRequest, generation) and GitHub's own highest pull-request number in X at that
# moment, the cursor `after` compares against — never a wall-clock timestamp, which skew between
# the operator and GitHub could fool. `after` PASSes only when (1) GitHub shows a pull request on
# `legion/<child>` in X, closed, whose number is above that cursor — proof the operation happened,
# not merely that nothing is open now — and (2) the daemon's own side is unaffected: the child is
# still recorded on the baseline's repository, and its pull request never names X.
negative_pr_in_x() {
  local phase=$1
  if [ "$phase" = before ]; then
    local child=$2
    local repo
    repo=$(negative_issue_repo_of pr-in-x "$child")
    local state_file
    state_file=$(state_raw)
    local record generation
    record=$(jq --arg k "$child" '.issues[$k] // null' "$state_file")
    generation=$(jq -r --arg k "$child" '.issues[$k].generation // -1' "$state_file")
    local highest_pr
    highest_pr=$(gh api "repos/$LEGION_E2E_REPO_X/pulls?state=all&per_page=100" --jq '[.[].number] | max // 0') ||
      fail "gh api repos/$LEGION_E2E_REPO_X/pulls failed"
    local extra
    extra=$(jq -n --argjson record "$record" --argjson generation "$generation" --argjson highest "$highest_pr" \
      '{record: $record, generation: $generation, highest_pr: $highest}')
    baseline_capture pr-in-x "$child" "$extra"
    return
  fi
  local run_id=$2 child=$3
  local baseline b_highest b_record_repo
  baseline=$(baseline_verify pr-in-x "$child" "$run_id")
  b_highest=$(jq -r '.highest_pr' <<<"$baseline")
  b_record_repo=$(jq -r '.record.repo // "none"' <<<"$baseline")

  local prs_file found_number
  prs_file="$MULTIREPO_EVIDENCE_DIR/negative-pr-in-x-$child-$run_id-pulls.raw"
  raw_fetch "$prs_file" gh api "repos/$LEGION_E2E_REPO_X/pulls?state=all&per_page=100"
  found_number=$(jq --arg ref "legion/$child" --argjson since "$b_highest" \
    '[.[] | select(.head.ref == $ref and .state == "closed" and .number > $since)] | length' "$prs_file")

  local state_file now_repo now_pr_repo ok=1
  state_file=$(state_raw)
  now_repo=$(jq -r --arg k "$child" '.issues[$k].repo // "none"' "$state_file")
  now_pr_repo=$(jq -r --arg k "$child" '.issues[$k].pullRequest.repo // "none"' "$state_file")
  { [ -n "${found_number:-}" ] && [ "$found_number" -gt 0 ]; } 2>/dev/null || ok=0
  [ "$(lower "$now_repo")" = "$(lower "$b_record_repo")" ] || ok=0
  [ "$(lower "$now_pr_repo")" != "$repo_x" ] || ok=0

  summary "negative-pr-in-x-$child" "baseline: repo=$b_record_repo highest_pr(X)=$b_highest
now: daemon repo=$now_repo, pullRequest.repo=$now_pr_repo
closed pull requests on legion/$child in X numbered above $b_highest: $found_number
---
$(cat "$prs_file")
" >/dev/null
  check "negative-pr-in-x-$child" "$ok"
}

# negative no-repo <KEY>: the daemon never recorded it (.issues has no such key — refusing the
# whole check, never a vacuous PASS, when .issues is not even an object, which `has` on `null`
# would otherwise answer `false` to), Dispatch itself still resolves it to no repository
# (.resolved_repo is null), and the latest repository note among the daemon's own messages (actor
# {kind:"session", id:"legion-daemon:<PROJECT>"} — never any other author's) is the exact
# no-repository admission refusal, word for word, bound to the daemon's own outbox marker
# (internal/workflow/repository.go:76, internal/record/outbox.go:71-73,104-106: body =
# note + "\n\n<!-- legion-outbox:<id> -->").
negative_no_repo() {
  local key=$1 ok=1 has
  local state_file
  state_file=$(state_raw)
  has=$(jq -r 'if (.issues | type) != "object" then "invalid" else (.issues | has($k)) end' --arg k "$key" "$state_file")
  [ "$has" = "false" ] || ok=0

  local detail_file
  detail_file=$(dispatch_issue_raw "$key")
  local resolved_repo
  resolved_repo=$(jq -r '.resolved_repo // "null"' "$detail_file")
  [ "$resolved_repo" = "null" ] || ok=0

  local events_file project daemon_actor
  events_file=$(dispatch_events_raw "$key")
  project=$(issue_project "$key")
  daemon_actor="legion-daemon:$project"
  local none_note="Legion did not admit this issue: its repository resolves to none. Set its repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name) and move it to todo again."
  local latest_is_none
  latest_is_none=$(jq --arg actor "$daemon_actor" --arg none "$none_note" '
    [.[] | select(.type=="message.created" and .actor.kind=="session" and .actor.id==$actor
      and ((.payload.body | startswith("Legion did not admit this issue:")) or (.payload.body | startswith("Legion keeps "))))]
    | sort_by(.seq) | last
    | if . == null then false else (.payload.body | startswith($none + "\n\n<!-- legion-outbox:")) end
  ' "$events_file")
  [ "$latest_is_none" = "true" ] || ok=0

  summary "negative-no-repo-$key" "issues has($key): $has
resolved_repo: $resolved_repo
daemon actor: $daemon_actor
latest daemon repository note is the none-note: $latest_is_none
" >/dev/null
  check "negative-no-repo-$key" "$ok"
}

# negative repo-edit before|after <run id>? <child KEY>: `before` captures the daemon's own
# recorded repository and refuses outright if Dispatch already resolves the issue to X (the
# control needs a real not-yet-edited baseline). `after` PASSes only when (1) the daemon's record
# is unchanged from the baseline, (2) Dispatch's own `.repo` for the issue now reads X — confirming
# the edit really happened there — (3) an `issue.updated` event's `.seq` is above the baseline's
# own last-seen `.seq` with `.payload.repo` equal to X (Dispatch's own sequence cursor, never a
# wall-clock comparison, and never merely "any issue.updated", which the daemon's own routine
# status writes also emit), and (4) the daemon's own exact kept-repository message
# (internal/workflow/repository.go:94) appears among its messages, word for word with the
# baseline's repository and X substituted in, bound to the daemon's own actor and outbox marker. A
# rerun needs no second message: `noteOnce` posts the kept-repository note once per resolution
# (internal/workflow/repository.go:101-113), so check (3) is this protocol's primary proof and (4)
# is the format checked, not required fresh.
negative_repo_edit() {
  local phase=$1
  if [ "$phase" = before ]; then
    local child=$2
    local repo
    repo=$(negative_issue_repo_of repo-edit "$child")
    local detail_file resolved_repo
    detail_file=$(dispatch_issue_raw "$child")
    resolved_repo=$(jq -r '.resolved_repo // "none"' "$detail_file")
    [ "$(lower "$resolved_repo")" != "$repo_x" ] || fail "negative repo-edit: $child already resolves to X ($resolved_repo) before any baseline; capture before only when the issue still resolves to T or C"
    local state_file generation
    state_file=$(state_raw)
    generation=$(jq -r --arg k "$child" '.issues[$k].generation // -1' "$state_file")
    local events_file last_seq
    events_file=$(dispatch_events_raw "$child")
    last_seq=$(jq '[.[].seq] | max // 0' "$events_file")
    local extra
    extra=$(jq -n --arg repo "$repo" --argjson generation "$generation" --argjson last_seq "$last_seq" \
      '{daemon_repo: $repo, generation: $generation, last_seq: $last_seq}')
    baseline_capture repo-edit "$child" "$extra"
    return
  fi
  local run_id=$2 child=$3
  local baseline b_repo b_last_seq
  baseline=$(baseline_verify repo-edit "$child" "$run_id")
  b_repo=$(jq -r '.daemon_repo' <<<"$baseline")
  b_last_seq=$(jq -r '.last_seq' <<<"$baseline")

  local state_file now_repo ok=1
  state_file=$(state_raw)
  now_repo=$(jq -r --arg k "$child" '.issues[$k].repo // "none"' "$state_file")
  [ "$(lower "$now_repo")" = "$(lower "$b_repo")" ] || ok=0

  local detail_file dispatch_repo_now
  detail_file=$(dispatch_issue_raw "$child")
  dispatch_repo_now=$(jq -r '.repo // "none"' "$detail_file")
  [ "$(lower "$dispatch_repo_now")" = "$repo_x" ] || ok=0

  local events_file edit_after
  events_file=$(dispatch_events_raw "$child")
  edit_after=$(jq --argjson since "$b_last_seq" --arg x "$repo_x" \
    '[.[] | select(.type=="issue.updated" and .seq > $since and ((.payload.repo // "") | ascii_downcase) == $x)] | length' "$events_file")
  { [ -n "${edit_after:-}" ] && [ "$edit_after" -gt 0 ]; } 2>/dev/null || ok=0

  local project daemon_actor note has_note
  project=$(issue_project "$child")
  daemon_actor="legion-daemon:$project"
  note="Legion keeps $b_repo as this issue's repository; the change to $LEGION_E2E_REPO_X in Dispatch is not applied — an issue's repository is fixed when Legion records it. Create a new issue for $LEGION_E2E_REPO_X instead."
  has_note=$(jq --arg actor "$daemon_actor" --arg note "$note" '
    any(.[]; .type=="message.created" and .actor.kind=="session" and .actor.id==$actor
      and (.payload.body | startswith($note + "\n\n<!-- legion-outbox:")))
  ' "$events_file")
  [ "$has_note" = "true" ] || ok=0

  summary "negative-repo-edit-$child" "baseline: repo=$b_repo last_seq=$b_last_seq
now: daemon repo=$now_repo, Dispatch repo=$dispatch_repo_now
issue.updated events with seq above baseline naming X: $edit_after
daemon's exact kept-repository message present: $has_note
" >/dev/null
  check "negative-repo-edit-$child" "$ok"
}

case "$1" in
  record-head) record_head "$2" ;;
  children) children "$2" ;;
  branch-base) branch_base "$2" ;;
  pin) pin "$2" "$3" ;;
  done)
    shift
    done_check "$@"
    ;;
  second-owner) second_owner "$2" "$3" "${4:-}" ;;
  cache) cache_check "$2" "$3" "${4:-}" ;;
  negative)
    case "$2" in
      pr-in-x) negative_pr_in_x "$3" "${4:-}" "${5:-}" ;;
      no-repo) negative_no_repo "$3" ;;
      repo-edit) negative_repo_edit "$3" "${4:-}" "${5:-}" ;;
    esac
    ;;
esac
