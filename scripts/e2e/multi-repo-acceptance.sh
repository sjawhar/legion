#!/usr/bin/env bash
# The acceptance observer for LEGION-1986's live multi-repository run, the operator's.
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
# the complete literal sentence, with the repository names Dispatch itself resolved substituted in,
# from a session actor named legion-daemon:<PROJECT> and carrying the daemon's outbox marker, never
# a loose substring over any author's messages. Dispatch takes a bearer session's id from the
# request body (packages/envoy/internal/dispatch/api/server.go, bearerSessionActor), so that actor
# id is the message's claimed author: whoever holds the project's Dispatch bearer can post the same
# text. The note shows what Dispatch holds, never who posted it; what shows the daemon declined an
# issue is its own state read.
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
#
# Optional, test seams only: `second-owner` takes three inputs a hermetic test replaces.
# MULTIREPO_PROC_ROOT is the `/proc`-shaped directory `as-role.py` scans for the role's own process
# (a pod always scans its own real `/proc`; a test builds a fake tree, since a shared devbox's own
# live panes would pollute the host's); MULTIREPO_TRUSTED_ROOT is the directory the image's
# absolute paths (`/opt/legion/bin/legion`, `/opt/legion/bin/gh`, `/var/run/legion/gh`) resolve
# under (empty in a pod); MULTIREPO_GITHUB_URL is GitHub's base URL (https://github.com in a pod).
# Each is refused unless MULTIREPO_TEST_SEAM=1 is set alongside it (an operator shell never sets
# any), and `second-owner`'s evidence names the value used, so a stray or malicious variable in a
# live run can neither redirect a probe nor be mistaken for a real result.
set -Eeuo pipefail
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

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
  # Every issue key goes into an evidence file name and an API path, so each is shaped like one
  # (PROJECT-123) before anything else happens. A key is the last argument of every negative form.
  local keys=() arg
  case "$1" in
    children | branch-base | second-owner | cache) keys=("$2") ;;
    done) keys=("${@:2}") ;;
    negative) keys=("${!#}") ;;
  esac
  for arg in "${keys[@]+"${keys[@]}"}"; do
    [[ $arg =~ ^[A-Z][A-Z0-9]*-[0-9]+$ ]] || {
      echo "usage: $(printf '%q' "$arg") is not an issue key (PROJECT-123)" >&2
      exit 2
    }
  done
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

# regex_escape escapes '.', the only regex metacharacter an owner/name shape (repo_shape_re) can
# contain, so a repository name is safe to embed literally in a grep -E pattern.
regex_escape() { printf '%s' "$1" | sed 's/\./\\./g'; }

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

# The Dispatch bearer never reaches argv (never a curl -H argument, which /proc/<curl>/cmdline and
# any echoed command line would then carry): it is written once, owner-only, into a curl config
# file every Dispatch read passes with -K, and removed on exit, interrupt or termination.
dispatch_curl_config=$(mktemp)
chmod 600 "$dispatch_curl_config"
printf 'header = "Authorization: Bearer %s"\n' "$dispatch_token" >"$dispatch_curl_config"

# arm_signal_traps installs the script's standing INT/TERM handlers: clean up, then exit with the
# shell's conventional 128+signal code, so an interrupted run both cleans up and actually stops —
# a bare `cleanup` with no `exit` would let the run press on after a SIGTERM into the
# next, unrelated read, which would then fail for the wrong reason instead of the script stopping at
# 143. raw_fetch below re-arms these once its own local traps are no longer needed.
arm_signal_traps() {
  trap 'cleanup; exit 130' INT
  trap 'cleanup; exit 143' TERM
}

cleanup() {
  rm -f "$dispatch_curl_config"
}
trap cleanup EXIT
arm_signal_traps

# raw_fetch <evidence file> <label> <command...> runs command with its stdout redirected straight
# to a fresh, uniquely-named temporary file (byte for byte, trailing bytes included — never through
# $(...), which strips trailing newlines and would leave the pin evidence short of the file's bytes),
# then renames it onto <evidence file> — a same-filesystem rename is atomic, so a second
# invocation of this script against the same evidence directory at the same time never leaves a
# reader mid-read of a truncated or interleaved file. A nonzero exit (the command's, or a failed
# rename — checked explicitly, since errexit alone does not catch it inside every caller's own
# $(...)) fails the whole observation, naming only <label> — never the command's own argv, which
# for a Dispatch read would otherwise print the bearer `-H` carries; curl's own
# stderr (a URL and a status, no credential) still reaches the terminal. The caller then reads
# <evidence file> itself, with jq's own filename argument, never a second `cat`/`$(...)` round trip.
#
# Most callers run inside a command-substitution subshell (`file=$(state_raw ...)`): a signal that
# reaches that subshell directly (a foreground Ctrl-C delivered to the whole process group) would
# otherwise leave its own temp file behind, since a plain variable the subshell sets never reaches
# the parent's trap. raw_fetch instead arms its own local INT/TERM traps around the fetch and the
# rename — whichever shell actually runs it, the main shell or a `$(...)` subshell — naming this
# call's own temp file literally (baked into the trap string with `printf %q` at set time, not
# looked up by name at signal time), then restores the script's standing handlers before
# returning, so a signal after this call behaves exactly as it did before it.
raw_fetch() {
  local file=$1 label=$2
  shift 2
  local tmp
  tmp=$(mktemp "$file.XXXXXX") || fail "could not create a temporary file for $file"
  local quoted_tmp
  quoted_tmp=$(printf '%q' "$tmp")
  # shellcheck disable=SC2064 # intentional: $quoted_tmp must bake in now, not re-resolve at the
  # signal, since another raw_fetch call may have changed or unset it by the time one fires.
  trap "rm -f $quoted_tmp; cleanup; exit 130" INT
  # shellcheck disable=SC2064
  trap "rm -f $quoted_tmp; cleanup; exit 143" TERM
  local status=0
  "$@" >"$tmp" || status=$?
  if [ "$status" -eq 0 ] && ! mv -f "$tmp" "$file"; then
    status=2
  fi
  arm_signal_traps
  case $status in
    0) ;;
    2)
      rm -f "$tmp"
      fail "$label: could not write $file"
      ;;
    *)
      rm -f "$tmp"
      fail "$label: failed"
      ;;
  esac
}

# state_raw <observation> refreshes $MULTIREPO_EVIDENCE_DIR/<observation>-state.raw from
# GET /legion/v1/state (no bearer: the route is open, the same bytes `legion state --json` prints)
# and prints its path. Named per observation (never a bare state.raw every observation shares), so
# a later observation's read never overwrites another observation's evidence.
state_raw() {
  local observation=$1
  local file="$MULTIREPO_EVIDENCE_DIR/$observation-state.raw"
  raw_fetch "$file" "GET $LEGION_E2E_DAEMON_URL/legion/v1/state" curl -sS --fail --max-time 30 "$LEGION_E2E_DAEMON_URL/legion/v1/state"
  printf '%s' "$file"
}

# dispatch_issue_raw <observation> <KEY> refreshes
# $MULTIREPO_EVIDENCE_DIR/<observation>-dispatch-issue-<KEY>.raw from Dispatch's own
# GET /api/v1/issues/<KEY> (the same JSON the dashboard reads, carrying .repo, .resolved_repo,
# .status, .generation and .children[] structurally — never `dispatch read`'s human prose) and
# prints its path.
dispatch_issue_raw() {
  local observation=$1 key=$2
  local file="$MULTIREPO_EVIDENCE_DIR/$observation-dispatch-issue-$key.raw"
  raw_fetch "$file" "GET $DISPATCH_URL/api/v1/issues/$key" curl -sS --fail --max-time 30 -K "$dispatch_curl_config" "$DISPATCH_URL/api/v1/issues/$key"
  printf '%s' "$file"
}

# dispatch_events_raw <observation> <KEY> refreshes
# $MULTIREPO_EVIDENCE_DIR/<observation>-dispatch-events-<KEY>.raw from Dispatch's own
# GET /api/v1/issues/<KEY>/events (newest first, up to 200): each event's .type, .seq, .actor and,
# for message.created, the complete .payload.body (never truncated, unlike `dispatch read`'s event
# summaries). Prints its path.
dispatch_events_raw() {
  local observation=$1 key=$2
  local file="$MULTIREPO_EVIDENCE_DIR/$observation-dispatch-events-$key.raw"
  raw_fetch "$file" "GET $DISPATCH_URL/api/v1/issues/$key/events" curl -sS --fail --max-time 30 -K "$dispatch_curl_config" \
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
# <observation>-<source>.raw file beside it, written byte for byte by raw_fetch.
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

# urlpath <value> percent-encodes value for a GitHub API path or query, keeping its slashes: a
# branch name such as release/v7.1 is legal, and so are characters (? # % space) that would end or
# change the request if they went in raw.
urlpath() {
  jq -rn --arg v "$1" '$v | @uri | gsub("%2F"; "/")'
}

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
  sha=$(gh api "repos/$repo/branches/$(urlpath "$default")" --jq .commit.sha) || fail "gh api repos/$repo/branches/$default failed"
  [[ $sha =~ $sha_re ]] || { ok=0; sha=""; }
  summary record-head "repo: $repo
default branch: $default
head: $sha
" >/dev/null
  echo "$sha"
  check record-head "$ok"
}

# children <parent KEY>: one child resolves to T and one to C, structurally, in both Dispatch's own
# record (.resolved_repo) and the daemon's state (.issues[<key>].repo). The daemon's state is read
# once, before any child's judgment, and that one read is what every child is judged against and
# what the kept evidence holds — never a fresh read per child, which could judge an early child
# against one state and a later child against another the daemon moved to meanwhile, while the
# evidence (one state.raw file per observation) could only ever hold the last of those reads,
# contradicting the judgment of any child read before it.
children() {
  local parent=$1 parent_file keys key child_file d_repo state_file s_repo saw_t=0 saw_c=0 mismatch=0 out=""
  parent_file=$(dispatch_issue_raw children "$parent")
  keys=$(jq -r '.children[].key' "$parent_file")
  [ -n "$keys" ] || fail "children: $parent has no children on Dispatch"
  state_file=$(state_raw children)
  while IFS= read -r key; do
    [ -n "$key" ] || continue
    child_file=$(dispatch_issue_raw children "$key")
    d_repo=$(jq -r '.resolved_repo // "none"' "$child_file")
    s_repo=$(jq -r --arg k "$key" '.issues[$k].repo // "none"' "$state_file")
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
  first_commit=$(gh api "repos/$LEGION_E2E_REPO_T/compare/$(urlpath "$default")...legion/$child" --jq '.commits[0].sha') ||
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

# pin <C> <T merge sha>: C's pin file, on C's default branch, pins exactly the merge sha: the one
# run of 40 or more hex digits it holds, compared whole, is the sha. A mention elsewhere (a comment,
# a "previous" line) beside a pin that names another commit, or a longer hex string the sha is only
# part of, never passes. Both arguments are validated before anything is fetched, and the file's own
# bytes go straight to evidence/pin.txt by redirection, never through a $(...) capture that would
# strip its trailing bytes.
pin() {
  local repo=$1 sha=$2 default content_file="$MULTIREPO_EVIDENCE_DIR/pin.txt" ok=0 pinned
  [ "$(lower "$repo")" = "$repo_c" ] || fail "pin's repository argument $repo is not LEGION_E2E_REPO_C ($LEGION_E2E_REPO_C)"
  [[ $sha =~ $sha_re ]] || fail "pin's merge-sha argument $(printf '%q' "$sha") is not a full 40-character commit id"
  default=$(default_branch "$repo")
  raw_fetch "$content_file.b64" "GET repos/$repo/contents/$LEGION_E2E_PIN_FILE" gh api "repos/$repo/contents/$LEGION_E2E_PIN_FILE?ref=$(urlpath "$default")" --jq .content
  base64 -d <"$content_file.b64" >"$content_file" || fail "pin: $LEGION_E2E_PIN_FILE's content is not valid base64"
  rm -f "$content_file.b64"
  [ -s "$content_file" ] || fail "pin: $LEGION_E2E_PIN_FILE on $repo@$default is empty"
  cat "$content_file"
  pinned=$(grep -oiE '[0-9a-f]{40,}' "$content_file" | tr 'A-F' 'a-f' | sort -u || true)
  [ "$pinned" = "$sha" ] && ok=1
  check pin "$ok"
}

# done <KEY>...: every named issue's Dispatch status (.status, structurally) is done.
done_check() {
  local key file out="" ok=1 status
  [ "$#" -gt 0 ] || fail "done: name at least one issue key"
  for key in "$@"; do
    file=$(dispatch_issue_raw "done" "$key")
    status=$(jq -r '.status' "$file")
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

# ---- running as the role, not the bare pod shell -------------------------------------------------
#
# `kubectl exec` carries none of a role's own variables, so a probe run that way cannot be the
# role's. lib/as-role.py resolves the role's agent process in the pod and runs the credential
# probes under that process's own environment; its header says how the agent is told apart from the
# shim's other children and why the probes are built as they are. It is streamed to `python3 -I -`
# in the role's container over `kubectl exec -i` and prints one JSON object, which this script keeps
# byte for byte as evidence and judges with jq. python3 is the pod's own: every role container's
# image is checked for it at boot (internal/capabilities/image.go).
as_role_script="$script_dir/lib/as-role.py"

# resolve_proc_root prints the /proc root as-role.py scans: the pod's own, except in a test, which
# must set MULTIREPO_TEST_SEAM=1 to redirect it to MULTIREPO_PROC_ROOT. MULTIREPO_PROC_ROOT alone is
# refused, so a stray or malicious variable in a live run never quietly redirects the scan.
resolve_proc_root() {
  if [ -n "${MULTIREPO_PROC_ROOT:-}" ]; then
    [ "${MULTIREPO_TEST_SEAM:-}" = 1 ] ||
      fail "MULTIREPO_PROC_ROOT is set without MULTIREPO_TEST_SEAM=1: this is a test-only seam, never a production input"
    printf '%s' "$MULTIREPO_PROC_ROOT"
    return
  fi
  printf '%s' /proc
}

# resolve_github_url prints the base URL as-role.py asks about push access: https://github.com,
# except in a test, which must set MULTIREPO_TEST_SEAM=1 to point it at a stub with
# MULTIREPO_GITHUB_URL (refused without the marker, as MULTIREPO_PROC_ROOT is).
resolve_github_url() {
  if [ -n "${MULTIREPO_GITHUB_URL:-}" ]; then
    [ "${MULTIREPO_TEST_SEAM:-}" = 1 ] ||
      fail "MULTIREPO_GITHUB_URL is set without MULTIREPO_TEST_SEAM=1: this is a test-only seam, never a production input"
    printf '%s' "$MULTIREPO_GITHUB_URL"
    return
  fi
  printf '%s' https://github.com
}

# resolve_trusted_root prints the directory the image's absolute paths (/opt/legion/bin/legion,
# /opt/legion/bin/gh, /usr/local/bin/gh, /var/run/legion/gh) resolve under in as-role.py: empty in a
# pod; a test lays out fakes under MULTIREPO_TRUSTED_ROOT (refused without MULTIREPO_TEST_SEAM=1).
resolve_trusted_root() {
  if [ -n "${MULTIREPO_TRUSTED_ROOT:-}" ]; then
    [ "${MULTIREPO_TEST_SEAM:-}" = 1 ] ||
      fail "MULTIREPO_TRUSTED_ROOT is set without MULTIREPO_TEST_SEAM=1: this is a test-only seam, never a production input"
    printf '%s' "$MULTIREPO_TRUSTED_ROOT"
  fi
}

# as_role_probe <pod> <container> <proc root> <github url> <trusted root> runs lib/as-role.py in the
# role's container with that file as stdin, and prints its JSON object.
as_role_probe() {
  local pod=$1 container=$2 proc_root=$3 github_url=$4 trusted_root=$5
  kubectl exec -i "$pod" -c "$container" -- python3 -I - \
    "$proc_root" "$LEGION_E2E_REPO_T" "$LEGION_E2E_REPO_O" "$o_owner" "$github_url" "$trusted_root" <"$as_role_script"
}

# second-owner <O child KEY> <pod> [container]: its pull request is in O (structurally,
# .pullRequest.repo) and GitHub confirms it merged there (gh api .../pulls/<n> --jq .merged; the
# daemon's `done` phase is accepted only beside `production_check`, never alone, since the phase is
# not merge proof by itself); the pod is bound to the issue by its legion.dev/issue label, as
# `cache` binds it. The credential evidence is lib/as-role.py's, run once under one resolved agent
# process: the agent's LEGION_GH_OWNERS_DIR, GH_CONFIG_DIR and LEGION_GH_PATH are the image's own
# (a difference fails: the pod is not the shape this check is about, or the agent rewrote its
# environment); `gh api repos/<T> --jq .full_name` reads T and says T; and GitHub's answer, with the
# credential the image's own `legion git-credential` hands out, to the request a `git push` opens
# with (`GET <repo>.git/info/refs?service=git-receive-pack`): for O, 200 and a receive-pack
# advertisement (the positive control: the network, the helper and the token are sound); for T, 403
# with GitHub's own whole-body refusal. That proves the credential the helper hands out for T
# cannot push T, which is the property; it is not a failure of some other kind, which an
# unreachable network or an unanswered helper would also give. The prober runs no git (git reads
# ~/.netrc, config files and the GIT_* variables, all of which the agent's uid can write) and takes
# no executable, search path or credential directory from the agent's environment, which its own
# code can rewrite in place: see the prober's header.
second_owner() {
  local child=$1 pod=$2 container=${3:-implementer} pr_repo pr_number phase merged ok=1
  command -v kubectl >/dev/null || fail "second-owner: kubectl is required and is not on PATH"
  require_pod_for_issue "$pod" "$child"

  local state_file
  state_file=$(state_raw second-owner)
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

  local proc_root github_url trusted_root probe_file
  proc_root=$(resolve_proc_root)
  github_url=$(resolve_github_url)
  trusted_root=$(resolve_trusted_root)
  probe_file="$MULTIREPO_EVIDENCE_DIR/second-owner-probe.raw"
  raw_fetch "$probe_file" "second-owner: the credential probes in $pod ($container)" \
    as_role_probe "$pod" "$container" "$proc_root" "$github_url" "$trusted_root"
  jq -e '
    (.pid | type == "string") and (.argv0 | type == "string") and (.argc | type == "number")
    and (.sha256 | type == "string") and (.env_match | type == "boolean")
    and (.read | type == "object" and (.status | type == "number") and (.output | type == "string"))
    and ([.push_o, .push_t] | all(type == "object" and (.status | type == "number")
      and (.content_type | type == "string") and (.output | type == "string")))
  ' "$probe_file" >/dev/null || fail "second-owner: the probe in $pod did not answer the object lib/as-role.py prints"

  local pid argv0 argc sha256 env_match read_out read_status
  local push_o_status push_o_type push_t_status push_t_body
  pid=$(jq -r .pid "$probe_file")
  argv0=$(jq -r .argv0 "$probe_file")
  argc=$(jq -r .argc "$probe_file")
  sha256=$(jq -r .sha256 "$probe_file")
  env_match=$(jq -r .env_match "$probe_file")
  read_out=$(jq -r .read.output "$probe_file")
  read_status=$(jq -r .read.status "$probe_file")
  push_o_status=$(jq -r .push_o.status "$probe_file")
  push_o_type=$(jq -r .push_o.content_type "$probe_file")
  push_t_status=$(jq -r .push_t.status "$probe_file")
  push_t_body=$(jq -r .push_t.output "$probe_file")

  [ "$env_match" = true ] || ok=0
  [ "$read_status" = 0 ] && [ "$(lower "$read_out")" = "$repo_t" ] || ok=0
  # The positive control: GitHub gives O's token the receive-pack advertisement, as it does a token
  # that can push.
  [ "$push_o_status" = 200 ] || ok=0
  case "$(lower "$push_o_type")" in
    application/x-git-receive-pack-advertisement*) ;;
    *) ok=0 ;;
  esac
  # The refusal: 403 with GitHub's own whole-body refusal — a token that cannot write ("Write access
  # to repository not granted.") or one the repository does not grant ("Permission to <T>.git denied
  # to <who>."), naming T. Any other status (a 404, a 5xx, 0 for no answer) or body (another
  # repository's refusal, extra lines) is not the refusal.
  local t_repo_re
  t_repo_re=$(regex_escape "$LEGION_E2E_REPO_T")
  [ "$push_t_status" = 403 ] || ok=0
  [ "$(printf '%s\n' "$push_t_body" | wc -l)" = 1 ] || ok=0
  printf '%s\n' "$push_t_body" | grep -qixE "(Write access to repository not granted\.|Permission to ${t_repo_re}\.git denied to [^[:space:]]+\.)" || ok=0

  summary second-owner "pullRequest.repo: $pr_repo
pullRequest.number: $pr_number
phase: $phase
merged (GitHub, repos/$LEGION_E2E_REPO_O/pulls/$pr_number): $merged
proc root: $proc_root
role pid: $pid
role cmdline: argv0=$argv0 argc=$argc sha256=$sha256
role env: the agent's LEGION_GH_OWNERS_DIR, GH_CONFIG_DIR and LEGION_GH_PATH are the image's: $env_match
trusted root: ${trusted_root:-<the image>}
GitHub: $github_url
as role: gh api repos/$LEGION_E2E_REPO_T --jq .full_name = $read_out (exit $read_status)
positive control: GET $LEGION_E2E_REPO_O.git/info/refs?service=git-receive-pack as role: HTTP $push_o_status, $push_o_type
negative probe: the same request for $LEGION_E2E_REPO_T as role: HTTP $push_t_status
---
$push_t_body
" >/dev/null
  check second-owner "$ok"
}

# cache <issue KEY> <pod> [container]: the named pod's legion.dev/issue label matches the issue,
# its fetch container's log carries the producer's exact, anchored cache-stream-with-delta success
# line (packages/daemon/cmd/legion/workspace_fetch.go:83, the form workspace_fetch_test.go:572
# pins, with the duration fields permissive enough for Go's own minute-and-above format, 1m2.345s),
# and the daemon's state shows that issue's repository's cache ready.
cache_success_re='^workspace-init fetch: [0-9,]+ bytes from the repository cache .+ in [0-9a-zµ.]+, its history verified in [0-9a-zµ.]+; [0-9,]+ bytes from GitHub for the delta \([0-9]+ refs?\)$'
cache_check() {
  local key=$1 pod=$2 container=${3:-workspace-fetch} repo ready logs_file ok=1
  command -v kubectl >/dev/null || fail "cache: kubectl is required and is not on PATH"
  require_pod_for_issue "$pod" "$key"
  logs_file="$MULTIREPO_EVIDENCE_DIR/cache-logs-$key.raw"
  raw_fetch "$logs_file" "kubectl logs $pod -c $container" kubectl logs "$pod" -c "$container"
  grep -qE "$cache_success_re" "$logs_file" || ok=0
  local state_file
  state_file=$(state_raw cache)
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

# baseline_verify <observation> <operation> <issue> <run id> reads and validates the baseline: a
# safe id, present, not foreign (operation, issue, project and daemon URL all match this call's,
# and the issue's current generation still equals the baseline's — a hand-over or re-admission
# since `before` invalidates the comparison), and not older than 24 hours (a negative control's
# operator action is expected to complete well within one working day; a much older file is a
# stale leftover from another session, never evidence for this one). Prints the baseline JSON on
# success.
baseline_verify() {
  local observation=$1 operation=$2 issue=$3 run_id=$4
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
  local state_file now_generation
  state_file=$(state_raw "$observation")
  now_generation=$(jq -r --arg k "$issue" '.issues[$k].generation // -1' "$state_file")
  [ "$now_generation" = "$b_generation" ] || fail "run $run_id's baseline was taken at generation $b_generation; $issue is now generation $now_generation (a hand-over or re-admission since): capture a fresh baseline"
  printf '%s' "$baseline"
}

# negative_issue_repo_of <observation> <control> <issue> requires issue already recorded on T or
# C, in the daemon's own state (.issues[<key>].repo), and returns it: the control needs a real
# recorded child to mean anything.
negative_issue_repo_of() {
  local observation=$1 control=$2 issue=$3 repo state_file
  state_file=$(state_raw "$observation")
  repo=$(jq -r --arg k "$issue" '.issues[$k].repo // "none"' "$state_file")
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
    repo=$(negative_issue_repo_of negative-pr-in-x-before pr-in-x "$child")
    local state_file
    state_file=$(state_raw negative-pr-in-x-before)
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
  baseline=$(baseline_verify negative-pr-in-x-after pr-in-x "$child" "$run_id")
  b_highest=$(jq -r '.highest_pr' <<<"$baseline")
  b_record_repo=$(jq -r '.record.repo // "none"' <<<"$baseline")

  local prs_file found_number
  prs_file="$MULTIREPO_EVIDENCE_DIR/negative-pr-in-x-after-$child-$run_id-pulls.raw"
  raw_fetch "$prs_file" "GET repos/$LEGION_E2E_REPO_X/pulls" gh api "repos/$LEGION_E2E_REPO_X/pulls?state=all&per_page=100"
  found_number=$(jq --arg ref "legion/$child" --argjson since "$b_highest" \
    '[.[] | select(.head.ref == $ref and .state == "closed" and .number > $since)] | length' "$prs_file")

  local state_file now_repo now_pr_repo ok=1
  state_file=$(state_raw negative-pr-in-x-after)
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
# (.resolved_repo is null), and the latest repository note among the messages claiming the
# daemon's actor (actor {kind:"session", id:"legion-daemon:<PROJECT>"}; the id is caller-named, see
# the header) is the exact no-repository admission refusal, word for word, with the daemon's outbox
# marker (internal/workflow/repository.go:76, internal/record/outbox.go:71-73,104-106: body =
# note + "\n\n<!-- legion-outbox:<id> -->").
negative_no_repo() {
  local key=$1 ok=1 has
  local state_file
  state_file=$(state_raw negative-no-repo)
  has=$(jq -r 'if (.issues | type) != "object" then "invalid" else (.issues | has($k)) end' --arg k "$key" "$state_file")
  [ "$has" = "false" ] || ok=0

  local detail_file
  detail_file=$(dispatch_issue_raw negative-no-repo "$key")
  local resolved_repo
  resolved_repo=$(jq -r '.resolved_repo // "null"' "$detail_file")
  [ "$resolved_repo" = "null" ] || ok=0

  local events_file project daemon_actor
  events_file=$(dispatch_events_raw negative-no-repo "$key")
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
actor id the note must carry (named by its poster, not token-proved): $daemon_actor
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
# (internal/workflow/repository.go:94) appears among its messages, word for word, with the
# baseline's own repository spelling and Dispatch's own resolved spelling of X substituted in —
# never the operator's own LEGION_E2E_REPO_X spelling, which may differ only in case from what
# Dispatch actually resolved and wrote — with the daemon's claimed actor id and outbox marker. A
# rerun needs no second message: `noteOnce` posts the kept-repository note once per resolution
# (internal/workflow/repository.go:101-113), so check (3) is this protocol's primary proof and (4)
# is the format checked, not required fresh.
negative_repo_edit() {
  local phase=$1
  if [ "$phase" = before ]; then
    local child=$2
    local repo
    repo=$(negative_issue_repo_of negative-repo-edit-before repo-edit "$child")
    local detail_file resolved_repo
    detail_file=$(dispatch_issue_raw negative-repo-edit-before "$child")
    resolved_repo=$(jq -r '.resolved_repo // "none"' "$detail_file")
    [ "$(lower "$resolved_repo")" != "$repo_x" ] || fail "negative repo-edit: $child already resolves to X ($resolved_repo) before any baseline; capture before only when the issue still resolves to T or C"
    local state_file generation
    state_file=$(state_raw negative-repo-edit-before)
    generation=$(jq -r --arg k "$child" '.issues[$k].generation // -1' "$state_file")
    local events_file last_seq
    events_file=$(dispatch_events_raw negative-repo-edit-before "$child")
    last_seq=$(jq '[.[].seq] | max // 0' "$events_file")
    local extra
    extra=$(jq -n --arg repo "$repo" --argjson generation "$generation" --argjson last_seq "$last_seq" \
      '{daemon_repo: $repo, generation: $generation, last_seq: $last_seq}')
    baseline_capture repo-edit "$child" "$extra"
    return
  fi
  local run_id=$2 child=$3
  local baseline b_repo b_last_seq
  baseline=$(baseline_verify negative-repo-edit-after repo-edit "$child" "$run_id")
  b_repo=$(jq -r '.daemon_repo' <<<"$baseline")
  b_last_seq=$(jq -r '.last_seq' <<<"$baseline")

  local state_file now_repo ok=1
  state_file=$(state_raw negative-repo-edit-after)
  now_repo=$(jq -r --arg k "$child" '.issues[$k].repo // "none"' "$state_file")
  [ "$(lower "$now_repo")" = "$(lower "$b_repo")" ] || ok=0

  local detail_file dispatch_repo_now
  detail_file=$(dispatch_issue_raw negative-repo-edit-after "$child")
  dispatch_repo_now=$(jq -r '.repo // "none"' "$detail_file")
  [ "$(lower "$dispatch_repo_now")" = "$repo_x" ] || ok=0

  local events_file edit_after
  events_file=$(dispatch_events_raw negative-repo-edit-after "$child")
  edit_after=$(jq --argjson since "$b_last_seq" --arg x "$repo_x" \
    '[.[] | select(.type=="issue.updated" and .seq > $since and ((.payload.repo // "") | ascii_downcase) == $x)] | length' "$events_file")
  { [ -n "${edit_after:-}" ] && [ "$edit_after" -gt 0 ]; } 2>/dev/null || ok=0

  local project daemon_actor note has_note
  project=$(issue_project "$child")
  daemon_actor="legion-daemon:$project"
  # The note's own repository names are Dispatch's own spellings (the baseline's kept repository,
  # $dispatch_repo_now for the one it refused), never the operator's LEGION_E2E_REPO_X, which the
  # case-insensitive comparisons above tolerate differing from but a literal quote must not.
  note="Legion keeps $b_repo as this issue's repository; the change to $dispatch_repo_now in Dispatch is not applied — an issue's repository is fixed when Legion records it. Create a new issue for $dispatch_repo_now instead."
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
