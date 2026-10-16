#!/usr/bin/env bash
# The acceptance observer for LEGION-1986's Task 6.4, the operator's live multi-repository run.
# It never starts a daemon, Dispatch, or any agent of its own, and it writes nothing: every
# admission, push, pull request and Dispatch write in the spec's acceptance checklist is the
# operator's and the agents' own. This script only reads the daemon's state, Dispatch and GitHub,
# and judges one observation at a time.
#
# Usage:
#   bash scripts/e2e/multi-repo-acceptance.sh record-head <T>
#   bash scripts/e2e/multi-repo-acceptance.sh children <parent KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh branch-base <T child KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh pin <C> <T merge sha>
#   bash scripts/e2e/multi-repo-acceptance.sh done <KEY>...
#   bash scripts/e2e/multi-repo-acceptance.sh second-owner <O child KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh cache <issue KEY> <pod> [container]
#   bash scripts/e2e/multi-repo-acceptance.sh negative pr-in-x <T child KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh negative no-repo <KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh negative repo-edit <child KEY>
#
# Required inputs (every one an operator input; the script refuses to start without any of
# them, naming the first missing one, and never carries a literal of its own):
#   LEGION_E2E_REPO_T       the toolchain repository, owner/name
#   LEGION_E2E_REPO_C       the consumer repository, owner/name
#   LEGION_E2E_PIN_FILE     C's pin file path (the path inside C that names T's pinned commit)
#   LEGION_E2E_REPO_X       the negative control's repository, owner/name
#   LEGION_E2E_REPO_O       the second owner's repository, owner/name
#   LEGION_E2E_DAEMON_URL   the daemon's API root (GET /legion/v1/state, GET /healthz)
#   DISPATCH_URL            the Dispatch server `dispatch` reads
#   DISPATCH_TOKEN_FILE     the Dispatch bearer token file `dispatch` reads
#   MULTIREPO_EVIDENCE_DIR  where each observation's evidence file is kept
#
# `negative pr-in-x` is run twice with the same key: the first run has no stored snapshot and
# takes one (before the operator opens the pull request in X); the second compares the stored
# snapshot against a fresh one (after that pull request is closed) and PASSes only when they are
# byte-equal.
set -Eeuo pipefail

self=$(basename "$0")

fail() {
  echo "$self: $1" >&2
  exit 1
}

usage() {
  cat >&2 <<EOF
usage: $self <observation> [args...]
  record-head <T>
  children <parent KEY>
  branch-base <T child KEY>
  pin <C> <T merge sha>
  done <KEY>...
  second-owner <O child KEY>
  cache <issue KEY> <pod> [container]
  negative pr-in-x <T child KEY>
  negative no-repo <KEY>
  negative repo-edit <child KEY>
EOF
  exit 2
}

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
require_env DISPATCH_URL "the Dispatch server's URL"
require_env DISPATCH_TOKEN_FILE "the Dispatch bearer token file"
require_env MULTIREPO_EVIDENCE_DIR "where this run's observation evidence is kept"

for pair in \
  "LEGION_E2E_REPO_T:LEGION_E2E_REPO_C" \
  "LEGION_E2E_REPO_T:LEGION_E2E_REPO_X" \
  "LEGION_E2E_REPO_T:LEGION_E2E_REPO_O" \
  "LEGION_E2E_REPO_C:LEGION_E2E_REPO_X" \
  "LEGION_E2E_REPO_C:LEGION_E2E_REPO_O" \
  "LEGION_E2E_REPO_X:LEGION_E2E_REPO_O"; do
  a=${pair%%:*} b=${pair#*:}
  [ "${!a}" != "${!b}" ] || fail "$a and $b both name ${!a}: the observations need distinct repositories"
done

t_owner=${LEGION_E2E_REPO_T%%/*}
o_owner=${LEGION_E2E_REPO_O%%/*}
[ "$t_owner" != "$o_owner" ] || fail "LEGION_E2E_REPO_T and LEGION_E2E_REPO_O share an owner ($t_owner): the second-owner observation needs two owners"

for tool in curl jq gh dispatch; do
  command -v "$tool" >/dev/null || fail "$tool is required and is not on PATH"
done

healthz_status=$(curl -sS -o /tmp/multi-repo-acceptance-healthz.$$ -w '%{http_code}' --max-time 15 "$LEGION_E2E_DAEMON_URL/healthz" 2>/dev/null) || healthz_status=000
healthz_body=$(cat /tmp/multi-repo-acceptance-healthz.$$ 2>/dev/null || true)
rm -f /tmp/multi-repo-acceptance-healthz.$$
[ "$healthz_status" = 200 ] && [ "$healthz_body" = '{"status":"ok"}' ] ||
  fail "LEGION_E2E_DAEMON_URL $LEGION_E2E_DAEMON_URL is not a Legion daemon: GET /healthz answered $healthz_status $healthz_body"

mkdir -p "$MULTIREPO_EVIDENCE_DIR"

# state_json is the one reading of the daemon's state every observation below that needs it uses,
# byte-for-byte what `legion state --json` prints (GET /legion/v1/state takes no bearer).
state_json() {
  curl -sS --fail --max-time 30 "$LEGION_E2E_DAEMON_URL/legion/v1/state"
}

# evidence <name> <text> writes the observation's evidence file and prints the text back, so a
# caller can both keep it and judge it in one call.
evidence() {
  local name=$1 text=$2
  printf '%b' "$text" >"$MULTIREPO_EVIDENCE_DIR/$name.txt"
  printf '%b' "$text"
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

# default_branch <owner/repo> is GitHub's own default branch name for it.
default_branch() {
  gh api "repos/$1" --jq .default_branch
}

# repository_of <dispatch read output> is the owner/name on its `Repository:` line, or `none`.
repository_of() {
  local line
  line=$(grep -m1 '^Repository: ' <<<"$1") || fail "the dispatch read output carries no Repository: line"
  case "$line" in
    "Repository: none") echo none ;;
    *) sed -E 's/^Repository: ([^ ]+).*/\1/' <<<"$line" ;;
  esac
}

# record-head <T>: the commit at T's default branch head, read now (run when T's child is
# recorded), stored for branch-base to compare the child branch's first commit's parent against.
record_head() {
  local repo=$1 default sha
  default=$(default_branch "$repo")
  sha=$(gh api "repos/$repo/branches/$default" --jq .commit.sha)
  evidence record-head "default branch: $default\nhead: $sha\n" >/dev/null
  echo "$sha"
  check record-head 1
}

# children <parent KEY>: one child on T, one on C, in both Dispatch and the daemon's state.
children() {
  local parent=$1 read keys key d_repo s_repo saw_t=0 saw_c=0 mismatch=0 out=""
  read=$(dispatch read --ref "dispatch://$parent/children")
  keys=$(grep -E '^- ' <<<"$read" | sed -E 's/^- ([A-Za-z0-9]+-[0-9]+):.*/\1/')
  [ -n "$keys" ] || fail "children: $parent has no children on Dispatch"
  while IFS= read -r key; do
    [ -n "$key" ] || continue
    d_repo=$(repository_of "$(dispatch read --issue "$key")")
    s_repo=$(state_json | jq -r --arg k "$key" '.issues[$k].repo // "none"')
    out+="$key: dispatch=$d_repo state=$s_repo"$'\n'
    [ "$d_repo" = "$s_repo" ] || mismatch=1
    [ "$d_repo" != "$LEGION_E2E_REPO_T" ] || saw_t=1
    [ "$d_repo" != "$LEGION_E2E_REPO_C" ] || saw_c=1
  done <<<"$keys"
  evidence children "$out" >/dev/null
  echo "$out"
  check children "$([ "$mismatch" = 0 ] && [ "$saw_t" = 1 ] && [ "$saw_c" = 1 ] && echo 1 || echo 0)"
}

# branch-base <T child KEY>: legion/<child>'s first commit's first parent is record-head's stored
# sha.
branch_base() {
  local child=$1 default stored_head first_commit parent_sha ok=0
  [ -s "$MULTIREPO_EVIDENCE_DIR/record-head.txt" ] || fail "branch-base: run record-head first"
  stored_head=$(grep '^head: ' "$MULTIREPO_EVIDENCE_DIR/record-head.txt" | cut -d' ' -f2)
  default=$(default_branch "$LEGION_E2E_REPO_T")
  first_commit=$(gh api "repos/$LEGION_E2E_REPO_T/compare/$default...legion/$child" --jq '.commits[0].sha')
  parent_sha=$(gh api "repos/$LEGION_E2E_REPO_T/commits/$first_commit" --jq '.parents[0].sha')
  [ "$parent_sha" != "$stored_head" ] || ok=1
  evidence branch-base "child: $child\nfirst commit: $first_commit\nfirst parent: $parent_sha\nrecord-head: $stored_head\n" >/dev/null
  check branch-base "$ok"
}

# pin <C> <T merge sha>: C's pin file names the merge sha.
pin() {
  local repo=$1 sha=$2 default content ok=0
  default=$(default_branch "$repo")
  content=$(gh api "repos/$repo/contents/$LEGION_E2E_PIN_FILE?ref=$default" --jq .content | base64 -d)
  evidence pin "$content" >/dev/null
  echo "$content"
  [[ $content != *"$sha"* ]] || ok=1
  check pin "$ok"
}

# done <KEY>...: every named issue reads Status: done on Dispatch.
done_check() {
  local key out="" ok=1
  [ "$#" -gt 0 ] || fail "done: name at least one issue key"
  for key in "$@"; do
    if grep -qx "Status: done" <<<"$(dispatch read --issue "$key")"; then
      out+="$key: done"$'\n'
    else
      out+="$key: not done"$'\n'
      ok=0
    fi
  done
  evidence "done" "$out" >/dev/null
  echo "$out"
  check "done" "$ok"
}

# second-owner <O child KEY>: its pull request names O, its phase is at or past production_check,
# and the issue's own messages record both the successful read of T and the refused push to T.
second_owner() {
  local child=$1 state pr_repo phase read ok=1
  state=$(state_json)
  pr_repo=$(jq -r --arg k "$child" '.issues[$k].pullRequest.repo // "none"' <<<"$state")
  phase=$(jq -r --arg k "$child" '.issues[$k].phase // "none"' <<<"$state")
  [ "$pr_repo" = "$LEGION_E2E_REPO_O" ] || ok=0
  case "$phase" in
    production_check | retro | awaiting_merge | done | merging) ;;
    *) ok=0 ;;
  esac
  read=$(dispatch read --issue "$child")
  grep -qi "$LEGION_E2E_REPO_T" <<<"$read" || ok=0
  grep -qi "refused" <<<"$read" || ok=0
  evidence second-owner "pullRequest.repo: $pr_repo\nphase: $phase\n---\n$read\n" >/dev/null
  check second-owner "$ok"
}

# cache <issue KEY> <pod> [container]: the fetch container's log names the cache stream and the
# GitHub delta, and the daemon's state shows the repository's cache ready.
cache_check() {
  local key=$1 pod=$2 container=${3:-workspace-fetch} state repo ready logs ok=1
  command -v kubectl >/dev/null || fail "cache: kubectl is required and is not on PATH"
  logs=$(kubectl logs "$pod" -c "$container")
  grep -qi "bytes from the repository cache" <<<"$logs" || ok=0
  grep -qi "github" <<<"$logs" || ok=0
  state=$(state_json)
  repo=$(jq -r --arg k "$key" '.issues[$k].repo // "none"' <<<"$state")
  ready=$(jq -r --arg r "$repo" '.repositories.mapped[] | select(.repo==$r) | .cache.ready' <<<"$state")
  [ "$ready" = true ] || ok=0
  evidence cache "issue: $key\nrepo: $repo\ncache.ready: $ready\n---\n$logs\n" >/dev/null
  check cache "$ok"
}

# negative pr-in-x <T child KEY>: twice-run snapshot comparison (see the usage note above).
negative_pr_in_x() {
  local child=$1 now ok
  local snapshot_file="$MULTIREPO_EVIDENCE_DIR/negative-pr-in-x-$child-before.json"
  now=$(state_json | jq --arg k "$child" '{issue: .issues[$k], pending: [.pendingStatusWrites[] | select(.issue==$k)]}')
  if [ ! -s "$snapshot_file" ]; then
    printf '%s' "$now" >"$snapshot_file"
    evidence "negative-pr-in-x-$child" "before snapshot taken; rerun this observation after the pull request in X closes\n" >/dev/null
    echo "snapshot taken; rerun after the pull request in X closes"
    check "negative-pr-in-x-$child" 1
    return
  fi
  if diff -q <(printf '%s' "$now") "$snapshot_file" >/dev/null; then
    ok=1
  else
    ok=0
  fi
  evidence "negative-pr-in-x-$child" "before:\n$(cat "$snapshot_file")\nafter:\n$now\n" >/dev/null
  check "negative-pr-in-x-$child" "$ok"
}

# negative no-repo <KEY>: the daemon never recorded it, and said why on the issue.
negative_no_repo() {
  local key=$1 state has read ok=1
  state=$(state_json)
  has=$(jq -r --arg k "$key" '.issues | has($k)' <<<"$state")
  [ "$has" = false ] || ok=0
  read=$(dispatch read --issue "$key")
  grep -qF "Legion did not admit this issue" <<<"$read" || ok=0
  evidence "negative-no-repo-$key" "issues has($key): $has\n---\n$read\n" >/dev/null
  check "negative-no-repo-$key" "$ok"
}

# negative repo-edit <child KEY>: the record keeps its original repository after the operator's
# edit, and said so.
negative_repo_edit() {
  local child=$1 state repo read ok=1
  state=$(state_json)
  repo=$(jq -r --arg k "$child" '.issues[$k].repo // "none"' <<<"$state")
  [ "$repo" = "$LEGION_E2E_REPO_T" ] || [ "$repo" = "$LEGION_E2E_REPO_C" ] || ok=0
  read=$(dispatch read --issue "$child")
  grep -qF "is not applied" <<<"$read" || ok=0
  evidence "negative-repo-edit-$child" "repo: $repo\n---\n$read\n" >/dev/null
  check "negative-repo-edit-$child" "$ok"
}

[ "$#" -ge 1 ] || usage

case "$1" in
  record-head)
    [ "$#" = 2 ] || usage
    record_head "$2"
    ;;
  children)
    [ "$#" = 2 ] || usage
    children "$2"
    ;;
  branch-base)
    [ "$#" = 2 ] || usage
    branch_base "$2"
    ;;
  pin)
    [ "$#" = 3 ] || usage
    pin "$2" "$3"
    ;;
  done)
    shift
    done_check "$@"
    ;;
  second-owner)
    [ "$#" = 2 ] || usage
    second_owner "$2"
    ;;
  cache)
    [ "$#" -ge 3 ] && [ "$#" -le 4 ] || usage
    cache_check "$2" "$3" "${4:-}"
    ;;
  negative)
    [ "$#" -ge 3 ] || usage
    case "$2" in
      pr-in-x)
        [ "$#" = 3 ] || usage
        negative_pr_in_x "$3"
        ;;
      no-repo)
        [ "$#" = 3 ] || usage
        negative_no_repo "$3"
        ;;
      repo-edit)
        [ "$#" = 3 ] || usage
        negative_repo_edit "$3"
        ;;
      *) usage ;;
    esac
    ;;
  *) usage ;;
esac
