#!/usr/bin/env bash
# The acceptance observer for LEGION-1986's Task 6.4, the operator's live multi-repository run.
# It never starts a daemon, Dispatch, or any agent of its own, and it writes nothing remote: every
# admission, push, pull request and Dispatch write in the spec's acceptance checklist is the
# operator's and the agents' own. This script only reads the daemon's state, Dispatch and GitHub,
# and judges one observation at a time, failing closed on every judgment: a subprocess's nonzero
# exit, an empty or malformed value, or a mismatch between the operator's named repository and the
# one the evidence actually names is a FAIL, never a PASS on a technicality.
#
# Usage:
#   bash scripts/e2e/multi-repo-acceptance.sh record-head <T>
#   bash scripts/e2e/multi-repo-acceptance.sh children <parent KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh branch-base <T child KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh pin <C> <T merge sha>
#   bash scripts/e2e/multi-repo-acceptance.sh done <KEY>...
#   bash scripts/e2e/multi-repo-acceptance.sh second-owner <O child KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh cache <issue KEY> <pod> [container]
#   bash scripts/e2e/multi-repo-acceptance.sh negative pr-in-x before|after <T child KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh negative no-repo <KEY>
#   bash scripts/e2e/multi-repo-acceptance.sh negative repo-edit before|after <child KEY>
#
# `negative pr-in-x` and `negative repo-edit` are two-call protocols, explicit about which call is
# which (never inferred from whether a snapshot file happens to exist): `before` takes and stores
# a snapshot, requiring the named issue to already be a recorded child of T or C; `after` compares
# a fresh snapshot against the stored one, requiring the same issue still recorded, and PASSes only
# when they are exactly equal (`pr-in-x`) or the repository is unchanged (`repo-edit`). Run `before`
# ahead of the operator's edit in X/Dispatch, and `after` once it is done.
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
# `dispatch` is the real Dispatch CLI (bundled by `@sjawhar/pi-envoy`), not a stand-in: it refuses
# to run without DISPATCH_HOST and a session id, and it keeps its own session-state root (ledger,
# memory, pruning) wherever DISPATCH_STATE_DIR points. This script sets DISPATCH_HOST=omp, a
# DISPATCH_SESSION_ID of its own, and DISPATCH_STATE_DIR under MULTIREPO_EVIDENCE_DIR, so every
# `dispatch` call it makes writes only inside this run's own evidence directory, never the shared
# session-state root an interactive agent uses.
set -Eeuo pipefail

self=${0##*/}

fail() {
  echo "$self: $1" >&2
  exit 1
}

# usage prints with the shell's own builtin echo, never `cat`: validate_argv must refuse before
# any external command is required, even when PATH is unset.
usage() {
  echo "usage: multi-repo-acceptance.sh <observation> [args...]" >&2
  echo "  record-head <T>" >&2
  echo "  children <parent KEY>" >&2
  echo "  branch-base <T child KEY>" >&2
  echo "  pin <C> <T merge sha>" >&2
  echo "  done <KEY>..." >&2
  echo "  second-owner <O child KEY>" >&2
  echo "  cache <issue KEY> <pod> [container]" >&2
  echo "  negative pr-in-x before|after <T child KEY>" >&2
  echo "  negative no-repo <KEY>" >&2
  echo "  negative repo-edit before|after <child KEY>" >&2
  exit 2
}

# validate_argv checks $1.. names a known observation with the right shape, before anything reads
# an environment variable, a tool, or the network: an absent or malformed observation is a usage
# error, not a dependency failure.
validate_argv() {
  [ "$#" -ge 1 ] || usage
  case "$1" in
    record-head | children | branch-base | second-owner) [ "$#" = 2 ] || usage ;;
    pin) [ "$#" = 3 ] || usage ;;
    done) [ "$#" -ge 2 ] || usage ;;
    cache) { [ "$#" = 3 ] || [ "$#" = 4 ]; } || usage ;;
    negative)
      [ "$#" -ge 2 ] || usage
      case "$2" in
        pr-in-x | repo-edit)
          [ "$#" = 4 ] || usage
          case "$3" in before | after) ;; *) usage ;; esac
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
require_env DISPATCH_URL "the Dispatch server's URL"
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

for tool in curl jq gh dispatch; do
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

# Isolate every `dispatch` call this script makes to this run's own evidence directory: the real
# CLI requires DISPATCH_HOST and a session id (it refuses to run without either), and keeps its
# ledger, memory and pruning under DISPATCH_STATE_DIR (default a shared root under the caller's
# HOME, which this script must never touch).
export DISPATCH_HOST=omp
export DISPATCH_SESSION_ID="multi-repo-acceptance-$$"
export DISPATCH_STATE_DIR="$MULTIREPO_EVIDENCE_DIR/dispatch-state"
mkdir -p "$DISPATCH_STATE_DIR"

# state_json is the one reading of the daemon's state every observation below that needs it uses,
# byte-for-byte what `legion state --json` prints (GET /legion/v1/state takes no bearer).
state_json() {
  curl -sS --fail --max-time 30 "$LEGION_E2E_DAEMON_URL/legion/v1/state"
}

# evidence <name> <text> writes the observation's evidence file verbatim (the external bytes it is
# given, never reinterpreted: no backslash escape expansion) and prints it back, so a caller can
# both keep it and judge it in one call.
evidence() {
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

# repository_of <dispatch read output> is the owner/name on its `Repository:` line, or `none`;
# fails the whole observation when the output carries no such line at all (a malformed or
# truncated read is not silently read as "none").
repository_of() {
  local line
  line=$(grep -m1 '^Repository: ' <<<"$1") || fail "the dispatch read output carries no Repository: line"
  case "$line" in
    "Repository: none") echo none ;;
    *) sed -E 's/^Repository: ([^ ]+).*/\1/' <<<"$line" ;;
  esac
}

# dispatch_read_issue <KEY> prints `dispatch read --issue <KEY>`'s output, or fails naming the
# nonzero exit: a failed call is never parsed for a value that happens to look right.
dispatch_read_issue() {
  local key=$1 out
  out=$(dispatch read --issue "$key") || fail "dispatch read --issue $key exited $?"
  printf '%s' "$out"
}

# record-head <T>: the commit at T's default branch head, read now (run when T's child is
# recorded), stored for branch-base to compare the child branch's first commit's parent against.
# Requires repo to be exactly T (case-insensitively) and the head to be a full commit id.
record_head() {
  local repo=$1 default sha ok=1
  [ "$(lower "$repo")" = "$repo_t" ] || fail "record-head's repository argument $repo is not LEGION_E2E_REPO_T ($LEGION_E2E_REPO_T)"
  default=$(default_branch "$repo")
  sha=$(gh api "repos/$repo/branches/$default" --jq .commit.sha) || fail "gh api repos/$repo/branches/$default failed"
  [[ $sha =~ $sha_re ]] || { ok=0; sha=""; }
  evidence record-head "repo: $repo
default branch: $default
head: $sha
" >/dev/null
  echo "$sha"
  check record-head "$ok"
}

# children <parent KEY>: one child on T, one on C, in both Dispatch and the daemon's state, both
# agreeing with each other; a failed dispatch read for any child fails the whole observation.
children() {
  local parent=$1 read keys key d_repo s_state s_repo saw_t=0 saw_c=0 mismatch=0 out=""
  read=$(dispatch read --ref "dispatch://$parent/children") || fail "dispatch read --ref dispatch://$parent/children exited $?"
  keys=$(grep -E '^- ' <<<"$read" | sed -E 's/^- ([A-Za-z0-9]+-[0-9]+):.*/\1/')
  [ -n "$keys" ] || fail "children: $parent has no children on Dispatch"
  while IFS= read -r key; do
    [ -n "$key" ] || continue
    d_repo=$(repository_of "$(dispatch_read_issue "$key")")
    s_state=$(state_json) || fail "GET $LEGION_E2E_DAEMON_URL/legion/v1/state failed"
    s_repo=$(jq -r --arg k "$key" '.issues[$k].repo // "none"' <<<"$s_state")
    out+="$key: dispatch=$d_repo state=$s_repo
"
    [ "$(lower "$d_repo")" = "$(lower "$s_repo")" ] || mismatch=1
    [ "$(lower "$d_repo")" != "$repo_t" ] || saw_t=1
    [ "$(lower "$d_repo")" != "$repo_c" ] || saw_c=1
  done <<<"$keys"
  evidence children "$out" >/dev/null
  printf '%s' "$out"
  check children "$([ "$mismatch" = 0 ] && [ "$saw_t" = 1 ] && [ "$saw_c" = 1 ] && echo 1 || echo 0)"
}

# branch-base <T child KEY>: legion/<child>'s first commit's first parent is record-head's stored
# sha, exactly (never an empty-vs-empty match: both sides must be full commit ids).
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
  evidence branch-base "child: $child
first commit: $first_commit
first parent: $parent_sha
record-head: $stored_head
" >/dev/null
  check branch-base "$ok"
}

# pin <C> <T merge sha>: C's pin file, on C's default branch, names the merge sha exactly; both the
# repository and the sha argument are validated before anything is fetched.
pin() {
  local repo=$1 sha=$2 default content ok=0
  [ "$(lower "$repo")" = "$repo_c" ] || fail "pin's repository argument $repo is not LEGION_E2E_REPO_C ($LEGION_E2E_REPO_C)"
  [[ $sha =~ $sha_re ]] || fail "pin's merge-sha argument $(printf '%q' "$sha") is not a full 40-character commit id"
  default=$(default_branch "$repo")
  content=$(gh api "repos/$repo/contents/$LEGION_E2E_PIN_FILE?ref=$default" --jq .content | base64 -d) ||
    fail "read repos/$repo/contents/$LEGION_E2E_PIN_FILE?ref=$default failed"
  [ -n "$content" ] || fail "pin: $LEGION_E2E_PIN_FILE on $repo@$default is empty"
  evidence pin "$content" >/dev/null
  printf '%s' "$content"
  [[ $content == *"$sha"* ]] && ok=1
  check pin "$ok"
}

# done <KEY>...: every named issue reads Status: done on Dispatch; a failed read fails the whole
# observation rather than being read as "not done" alongside a real failure.
done_check() {
  local key out="" ok=1 read
  [ "$#" -gt 0 ] || fail "done: name at least one issue key"
  for key in "$@"; do
    read=$(dispatch_read_issue "$key")
    if grep -qx "Status: done" <<<"$read"; then
      out+="$key: done
"
    else
      out+="$key: not done
"
      ok=0
    fi
  done
  evidence "done" "$out" >/dev/null
  printf '%s' "$out"
  check "done" "$ok"
}

# second-owner <O child KEY>: its pull request names O, its phase is production_check or done (the
# planned post-merge phases — never merging/awaiting_merge/retro, which precede the merge), and the
# issue's own messages separately record a successful `gh api repos/<T>` read and a refused `git
# push` to T, as two distinct lines rather than two substrings anywhere in the text.
second_owner() {
  local child=$1 state pr_repo phase read ok=1
  state=$(state_json) || fail "GET $LEGION_E2E_DAEMON_URL/legion/v1/state failed"
  pr_repo=$(jq -r --arg k "$child" '.issues[$k].pullRequest.repo // "none"' <<<"$state")
  phase=$(jq -r --arg k "$child" '.issues[$k].phase // "none"' <<<"$state")
  [ "$(lower "$pr_repo")" = "$repo_o" ] || ok=0
  case "$phase" in
    production_check | done) ;;
    *) ok=0 ;;
  esac
  read=$(dispatch_read_issue "$child")
  grep -qiE "gh api repos/${LEGION_E2E_REPO_T//\//\\/}[^$'\n']*(succeed|success|ok|200)" <<<"$read" || ok=0
  grep -qiE "git push[^$'\n']*${LEGION_E2E_REPO_T//\//\\/}[^$'\n']*refus" <<<"$read" || ok=0
  evidence second-owner "pullRequest.repo: $pr_repo
phase: $phase
---
$read
" >/dev/null
  check second-owner "$ok"
}

# cache <issue KEY> <pod> [container]: the named pod's label legion.dev/issue matches the named
# issue (so the operator cannot point the observation at an unrelated pod's successful fetch), its
# fetch container's log carries the producer's exact cache-stream-with-delta success line
# (workspace_fetch.go's one `report.CacheBytes > 0` branch, not merely the words "cache" and
# "github" anywhere, which an error line also contains), and the daemon's state shows that issue's
# repository's cache ready.
cache_check() {
  local key=$1 pod=$2 container=${3:-workspace-fetch} state repo ready logs pod_issue ok=1
  command -v kubectl >/dev/null || fail "cache: kubectl is required and is not on PATH"
  pod_issue=$(kubectl get pod "$pod" -o jsonpath='{.metadata.labels.legion\.dev/issue}' 2>/dev/null) ||
    fail "kubectl get pod $pod failed: cannot confirm it is $key's pod"
  [ "$pod_issue" = "$key" ] || fail "pod $pod's legion.dev/issue label is $(printf '%q' "$pod_issue"), not $key: this is not that issue's pod"
  logs=$(kubectl logs "$pod" -c "$container") || fail "kubectl logs $pod -c $container failed"
  grep -qE 'workspace-init fetch: [0-9,]+ bytes from the repository cache .*; [0-9,]+ bytes from GitHub for the delta' <<<"$logs" || ok=0
  state=$(state_json) || fail "GET $LEGION_E2E_DAEMON_URL/legion/v1/state failed"
  repo=$(jq -r --arg k "$key" '.issues[$k].repo // "none"' <<<"$state")
  [ "$repo" != "none" ] || ok=0
  ready=$(jq -r --arg r "$repo" '.repositories.mapped[] | select((.repo|ascii_downcase)==($r|ascii_downcase)) | .cache.ready' <<<"$state")
  [ "$ready" = true ] || ok=0
  evidence cache "issue: $key
pod: $pod ($container), legion.dev/issue=$pod_issue
repo: $repo
cache.ready: $ready
---
$logs
" >/dev/null
  check cache "$ok"
}

# negative_pr_in_x_snapshot <child> is the comparable state for that negative control: the issue's
# own record plus its pending status writes, requiring the issue to be a recorded child of T or C
# (never null — a nonexistent issue proves nothing about a pull request opened and closed in X).
negative_pr_in_x_snapshot() {
  local child=$1 state issue_repo snap
  state=$(state_json) || fail "GET $LEGION_E2E_DAEMON_URL/legion/v1/state failed"
  issue_repo=$(jq -r --arg k "$child" '.issues[$k].repo // "none"' <<<"$state")
  { [ "$(lower "$issue_repo")" = "$repo_t" ] || [ "$(lower "$issue_repo")" = "$repo_c" ]; } ||
    fail "negative pr-in-x: $child's recorded repository is $issue_repo, neither T ($LEGION_E2E_REPO_T) nor C ($LEGION_E2E_REPO_C): not the recorded child this control needs"
  snap=$(jq --arg k "$child" '{issue: .issues[$k], pending: [.pendingStatusWrites[] | select(.issue==$k)]}' <<<"$state")
  printf '%s' "$snap"
}

# negative pr-in-x before|after <T child KEY>: see the usage note above for the two-call protocol.
negative_pr_in_x() {
  local phase=$1 child=$2 now ok
  local snapshot_file="$MULTIREPO_EVIDENCE_DIR/negative-pr-in-x-$child-before.json"
  if [ "$phase" = before ]; then
    now=$(negative_pr_in_x_snapshot "$child")
    printf '%s' "$now" >"$snapshot_file"
    evidence "negative-pr-in-x-$child" "before snapshot (the recorded child's issue + pendingStatusWrites):
$now
" >/dev/null
    check "negative-pr-in-x-$child" 1
    return
  fi
  [ -s "$snapshot_file" ] || fail "negative pr-in-x after: no before snapshot for $child; run 'negative pr-in-x before $child' first"
  now=$(negative_pr_in_x_snapshot "$child")
  if diff -q <(printf '%s' "$now") "$snapshot_file" >/dev/null; then ok=1; else ok=0; fi
  evidence "negative-pr-in-x-$child" "before:
$(cat "$snapshot_file")
after:
$now
" >/dev/null
  check "negative-pr-in-x-$child" "$ok"
}

# negative no-repo <KEY>: the daemon never recorded it, Dispatch itself shows the issue resolves to
# no repository (`Repository: none`, not merely an absence from state, which a daemon outage would
# also produce), and the issue's messages hold the specific no-repository admission refusal.
negative_no_repo() {
  local key=$1 state has read ok=1
  state=$(state_json) || fail "GET $LEGION_E2E_DAEMON_URL/legion/v1/state failed"
  has=$(jq -r --arg k "$key" '.issues | has($k)' <<<"$state")
  [ "$has" = false ] || ok=0
  read=$(dispatch_read_issue "$key")
  grep -qx "Repository: none" <<<"$read" || ok=0
  grep -qiE "Legion did not admit this issue.*repository.*(none|resolves to none)" <<<"$read" || ok=0
  evidence "negative-no-repo-$key" "issues has($key): $has
---
$read
" >/dev/null
  check "negative-no-repo-$key" "$ok"
}

# negative_repo_edit_repo <child> is the repository negative-repo-edit compares, requiring the
# issue to be a recorded child of T or C.
negative_repo_edit_repo() {
  local child=$1 state repo
  state=$(state_json) || fail "GET $LEGION_E2E_DAEMON_URL/legion/v1/state failed"
  repo=$(jq -r --arg k "$child" '.issues[$k].repo // "none"' <<<"$state")
  { [ "$(lower "$repo")" = "$repo_t" ] || [ "$(lower "$repo")" = "$repo_c" ]; } ||
    fail "negative repo-edit: $child's recorded repository is $repo, neither T ($LEGION_E2E_REPO_T) nor C ($LEGION_E2E_REPO_C): not the recorded child this control needs"
  printf '%s' "$repo"
}

# negative repo-edit before|after <child KEY>: the record's repository is unchanged across the
# operator's edit, and the issue's messages record that edit targeted X specifically and was not
# applied.
negative_repo_edit() {
  local phase=$1 child=$2 original now ok
  local snapshot_file="$MULTIREPO_EVIDENCE_DIR/negative-repo-edit-$child-before.txt"
  if [ "$phase" = before ]; then
    original=$(negative_repo_edit_repo "$child")
    printf '%s' "$original" >"$snapshot_file"
    evidence "negative-repo-edit-$child" "before: repo=$original
" >/dev/null
    check "negative-repo-edit-$child" 1
    return
  fi
  [ -s "$snapshot_file" ] || fail "negative repo-edit after: no before snapshot for $child; run 'negative repo-edit before $child' first"
  original=$(cat "$snapshot_file")
  now=$(negative_repo_edit_repo "$child")
  ok=1
  [ "$(lower "$now")" = "$(lower "$original")" ] || ok=0
  local read
  read=$(dispatch_read_issue "$child")
  grep -qiE "repo[^$'\n']*${LEGION_E2E_REPO_X//\//\\/}[^$'\n']*is not applied" <<<"$read" || ok=0
  evidence "negative-repo-edit-$child" "before: repo=$original
after:  repo=$now
---
$read
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
  second-owner) second_owner "$2" ;;
  cache) cache_check "$2" "$3" "${4:-}" ;;
  negative)
    case "$2" in
      pr-in-x) negative_pr_in_x "$3" "$4" ;;
      no-repo) negative_no_repo "$3" ;;
      repo-edit) negative_repo_edit "$3" "$4" ;;
    esac
    ;;
esac
