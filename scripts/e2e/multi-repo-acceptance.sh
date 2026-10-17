#!/usr/bin/env bash
# The acceptance observer for LEGION-1986's Task 6.4, the operator's live multi-repository run.
# It never starts a daemon, Dispatch, or any agent of its own, and it writes nothing remote: every
# admission, push, pull request and Dispatch write in the spec's acceptance checklist is the
# operator's and the agents' own.
#
# Every judgment reads a structured source: the daemon's state JSON (GET /legion/v1/state),
# Dispatch's own JSON API (GET /api/v1/issues/<key> and /events, the same routes the dashboard
# reads — not `dispatch read`'s human prose, which truncates and has no stable grammar), GitHub's
# API (`gh api ... --jq`), and `kubectl get`/`logs`/`exec` as the operator. The one exception is a
# daemon message whose exact format the daemon itself produces (quoted below with its file:line):
# those are matched as the complete literal sentence, with the repository names the run supplies
# substituted in, never a loose substring.
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
# judgment. `after` takes that exact run id back, refuses a missing, foreign (another issue,
# operation, or daemon), or stale (more than 24 hours old) baseline, and PASSes only once GitHub's
# or Dispatch's own structured record shows the negative control's operation demonstrably happened
# after the baseline was taken.
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

# state_json is the one reading of the daemon's state every observation below that needs it uses,
# byte-for-byte what `legion state --json` prints (GET /legion/v1/state takes no bearer).
state_json() {
  curl -sS --fail --max-time 30 "$LEGION_E2E_DAEMON_URL/legion/v1/state"
}

# dispatch_issue <KEY> is Dispatch's own GET /api/v1/issues/<KEY> (the same JSON the dashboard
# reads), carrying .repo, .resolved_repo, .status, and .children[] structurally — never the `dispatch
# read` CLI's human prose, which truncates and has no stable grammar to parse. A nonzero exit (a
# network failure, or Dispatch answering anything but 2xx) fails the whole observation.
dispatch_issue() {
  local key=$1
  curl -sS --fail --max-time 30 -H "Authorization: Bearer $dispatch_token" "$DISPATCH_URL/api/v1/issues/$key" ||
    fail "GET $DISPATCH_URL/api/v1/issues/$key failed"
}

# dispatch_events <KEY> is Dispatch's own GET /api/v1/issues/<KEY>/events (newest first, up to
# 200): each event's .type, .created_at and, for message.created, the complete .payload.body (never
# truncated, unlike `dispatch read`'s event summaries).
dispatch_events() {
  local key=$1
  curl -sS --fail --max-time 30 -H "Authorization: Bearer $dispatch_token" \
    "$DISPATCH_URL/api/v1/issues/$key/events?order=desc&limit=200" ||
    fail "GET $DISPATCH_URL/api/v1/issues/$key/events failed"
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

# record-head <T>: the commit at T's default branch head, read now (run when T's child is
# recorded), stored for branch-base to compare the child branch's first commit's parent against.
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

# children <parent KEY>: one child resolves to T and one to C, structurally, in both Dispatch's own
# record (.resolved_repo) and the daemon's state (.issues[<key>].repo).
children() {
  local parent=$1 detail keys key d_repo s_state s_repo saw_t=0 saw_c=0 mismatch=0 out=""
  detail=$(dispatch_issue "$parent")
  keys=$(jq -r '.children[].key' <<<"$detail")
  [ -n "$keys" ] || fail "children: $parent has no children on Dispatch"
  while IFS= read -r key; do
    [ -n "$key" ] || continue
    d_repo=$(dispatch_issue "$key" | jq -r '.resolved_repo // "none"')
    s_state=$(state_json)
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

# done <KEY>...: every named issue's Dispatch status (.status, structurally) is done.
done_check() {
  local key out="" ok=1 status
  [ "$#" -gt 0 ] || fail "done: name at least one issue key"
  for key in "$@"; do
    status=$(dispatch_issue "$key" | jq -r '.status')
    if [ "$status" = "done" ]; then
      out+="$key: done
"
    else
      out+="$key: $status
"
      ok=0
    fi
  done
  evidence "done" "$out" >/dev/null
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

# second-owner <O child KEY> <pod> [container]: its pull request is in O (structurally,
# .pullRequest.repo) and GitHub confirms it merged there (gh api .../pulls/<n> --jq .merged); the
# credential evidence is no longer prose — it is run live, through `kubectl exec` as the operator,
# inside the issue's own role container: a read of T succeeds (`gh api repos/<T> --jq .full_name`
# must equal T exactly) and a dry-run push to T is refused (nonzero exit; a dry-run push only talks
# to receive-pack's advertisement, so it writes nothing to T).
second_owner() {
  local child=$1 pod=$2 container=${3:-implementer} state pr_repo pr_number phase merged ok=1
  command -v kubectl >/dev/null || fail "second-owner: kubectl is required and is not on PATH"
  require_pod_for_issue "$pod" "$child"
  state=$(state_json)
  pr_repo=$(jq -r --arg k "$child" '.issues[$k].pullRequest.repo // "none"' <<<"$state")
  pr_number=$(jq -r --arg k "$child" '.issues[$k].pullRequest.number // empty' <<<"$state")
  phase=$(jq -r --arg k "$child" '.issues[$k].phase // "none"' <<<"$state")
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
  local read_full_name
  read_full_name=$(kubectl exec "$pod" -c "$container" -- gh api "repos/$LEGION_E2E_REPO_T" --jq .full_name 2>/dev/null) || read_full_name=""
  [ "$(lower "$read_full_name")" = "$repo_t" ] || ok=0
  local probe_branch
  probe_branch="legion-probe-$$-$(date +%s)"
  local push_status=0
  kubectl exec "$pod" -c "$container" -- git push --dry-run "https://github.com/$LEGION_E2E_REPO_T" "HEAD:refs/heads/$probe_branch" >/dev/null 2>&1 || push_status=$?
  [ "$push_status" != 0 ] || ok=0
  evidence second-owner "pullRequest.repo: $pr_repo
pullRequest.number: $pr_number
phase: $phase
merged (GitHub): $merged
read repos/$LEGION_E2E_REPO_T --jq .full_name (in-pod): $read_full_name
dry-run push to $LEGION_E2E_REPO_T (in-pod) exit status: $push_status
" >/dev/null
  check second-owner "$ok"
}

# cache <issue KEY> <pod> [container]: the named pod's legion.dev/issue label matches the issue,
# its fetch container's log carries the producer's exact cache-stream-with-delta success line
# (packages/daemon/cmd/legion/workspace_fetch.go's one `report.CacheBytes > 0` branch), and the
# daemon's state shows that issue's repository's cache ready.
cache_check() {
  local key=$1 pod=$2 container=${3:-workspace-fetch} state repo ready logs ok=1
  command -v kubectl >/dev/null || fail "cache: kubectl is required and is not on PATH"
  require_pod_for_issue "$pod" "$key"
  logs=$(kubectl logs "$pod" -c "$container") || fail "kubectl logs $pod -c $container failed"
  grep -qE 'workspace-init fetch: [0-9,]+ bytes from the repository cache .*; [0-9,]+ bytes from GitHub for the delta' <<<"$logs" || ok=0
  state=$(state_json)
  repo=$(jq -r --arg k "$key" '.issues[$k].repo // "none"' <<<"$state")
  [ "$repo" != "none" ] || ok=0
  ready=$(jq -r --arg r "$repo" '.repositories.mapped[] | select((.repo|ascii_downcase)==($r|ascii_downcase)) | .cache.ready' <<<"$state")
  [ "$ready" = true ] || ok=0
  evidence cache "issue: $key
pod: $pod ($container)
repo: $repo
cache.ready: $ready
---
$logs
" >/dev/null
  check cache "$ok"
}

# ---- the two-call baseline protocol (negative pr-in-x, negative repo-edit) ------------------

# baseline_dir <run id> is the per-run directory a baseline lives under: unique per invocation of
# `before`, so two overlapping captures can never collide or overwrite each other.
baseline_dir() {
  printf '%s/runs/%s' "$MULTIREPO_EVIDENCE_DIR" "$1"
}

# baseline_capture <operation> <issue> <extra JSON object> mints a fresh run id, creates its
# directory and its baseline file exclusively (mkdir, then `set -o noclobber`, both of which fail
# loudly rather than overwrite), and prints the run id and a BASELINE line — never PASS: capturing
# a baseline observes nothing about the negative control yet.
baseline_capture() {
  local operation=$1 issue=$2 extra=$3
  local run_id
  run_id="${operation}-${issue}-$(date -u +%Y%m%dT%H%M%SZ)-$$-$RANDOM"
  local dir
  dir=$(baseline_dir "$run_id")
  mkdir -p "$MULTIREPO_EVIDENCE_DIR/runs"
  mkdir "$dir" 2>/dev/null || fail "a baseline directory for run $run_id already exists; this should be impossible for a freshly minted run id"
  local started_at
  started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  local payload
  payload=$(jq -n --arg run "$run_id" --arg op "$operation" --arg issue "$issue" \
    --arg daemon "$LEGION_E2E_DAEMON_URL" --arg started "$started_at" --argjson extra "$extra" \
    '{run_id: $run, operation: $op, issue: $issue, daemon_url: $daemon, started_at: $started} + $extra')
  (
    set -o noclobber
    printf '%s' "$payload" >"$dir/baseline.json"
  ) 2>/dev/null || fail "the baseline file for run $run_id already exists; this should be impossible for a freshly minted run id"
  printf '%s' "$payload" >"$MULTIREPO_EVIDENCE_DIR/negative-$operation-$issue-$run_id.txt"
  echo "RUN_ID: $run_id"
  echo "BASELINE $operation-$issue: captured"
}

# baseline_verify <operation> <issue> <run id> reads and validates the baseline: present, not
# foreign (operation, issue and daemon URL all match this call's), and not older than 24 hours (a
# negative control's operator action is expected to complete well within one working day; a much
# older file is a stale leftover from another session, never evidence for this one). Prints the
# baseline JSON on success.
baseline_verify() {
  local operation=$1 issue=$2 run_id=$3 dir
  dir=$(baseline_dir "$run_id")
  [ -f "$dir/baseline.json" ] || fail "no baseline for run $run_id: run 'negative $operation before $issue' first"
  local baseline
  baseline=$(cat "$dir/baseline.json")
  local b_op b_issue b_daemon b_started
  b_op=$(jq -r '.operation' <<<"$baseline")
  b_issue=$(jq -r '.issue' <<<"$baseline")
  b_daemon=$(jq -r '.daemon_url' <<<"$baseline")
  b_started=$(jq -r '.started_at' <<<"$baseline")
  [ "$b_op" = "$operation" ] || fail "run $run_id's baseline is for operation $b_op, not $operation: a foreign baseline"
  [ "$b_issue" = "$issue" ] || fail "run $run_id's baseline is for issue $b_issue, not $issue: a foreign baseline"
  [ "$b_daemon" = "$LEGION_E2E_DAEMON_URL" ] || fail "run $run_id's baseline is against $b_daemon, not $LEGION_E2E_DAEMON_URL: a foreign baseline"
  local started_epoch now_epoch
  started_epoch=$(date -u -d "$b_started" +%s 2>/dev/null) || fail "run $run_id's baseline has an unreadable started_at ($b_started)"
  now_epoch=$(date -u +%s)
  [ $((now_epoch - started_epoch)) -le 86400 ] || fail "run $run_id's baseline is more than 24h old (captured $b_started): too stale to trust; capture a fresh one with 'negative $operation before $issue'"
  printf '%s' "$baseline"
}

# negative_pr_in_x_repo requires child already recorded on T or C, in the daemon's own state
# (.issues[<key>].repo), and returns it: the control needs a real recorded child to mean anything.
negative_issue_repo_of() {
  local control=$1 child=$2 state repo
  state=$(state_json)
  repo=$(jq -r --arg k "$child" '.issues[$k].repo // "none"' <<<"$state")
  { [ "$(lower "$repo")" = "$repo_t" ] || [ "$(lower "$repo")" = "$repo_c" ]; } ||
    fail "negative $control: $child's recorded repository is $repo, neither T ($LEGION_E2E_REPO_T) nor C ($LEGION_E2E_REPO_C): not the recorded child this control needs"
  printf '%s' "$repo"
}

# negative pr-in-x before|after <run id>? <T child KEY>: the baseline is the issue's record and
# pendingStatusWrites; `after` PASSes only when GitHub's own record of X shows a pull request whose
# head is legion/<child>, created and then closed strictly after the baseline's timestamp — proof
# the operator's open-then-close in X actually happened, not merely that nothing is currently open.
negative_pr_in_x() {
  local phase=$1
  if [ "$phase" = before ]; then
    local child=$2
    negative_issue_repo_of pr-in-x "$child" >/dev/null
    local state snap
    state=$(state_json)
    snap=$(jq -n --argjson state "$state" --arg k "$child" \
      '{record: ($state.issues[$k] // null), pending: [$state.pendingStatusWrites[] | select(.issue==$k)]}')
    baseline_capture pr-in-x "$child" "$snap"
    return
  fi
  local run_id=$2 child=$3
  local baseline b_started
  baseline=$(baseline_verify pr-in-x "$child" "$run_id")
  b_started=$(jq -r '.started_at' <<<"$baseline")
  local prs found
  prs=$(gh api "repos/$LEGION_E2E_REPO_X/pulls?state=all&per_page=100" --jq '.') || fail "gh api repos/$LEGION_E2E_REPO_X/pulls failed"
  found=$(jq --arg ref "legion/$child" --arg since "$b_started" \
    '[.[] | select(.head.ref == $ref and .state == "closed" and .created_at > $since and ((.closed_at // "") > $since))] | length' <<<"$prs")
  local ok=0
  [ "${found:-0}" -gt 0 ] 2>/dev/null && ok=1
  evidence "negative-pr-in-x-$child-$run_id" "baseline started_at: $b_started
matching closed pull requests on legion/$child in $LEGION_E2E_REPO_X opened and closed after the baseline: $found
---
$prs
" >/dev/null
  check "negative-pr-in-x-$child" "$ok"
}

# negative no-repo <KEY>: the daemon never recorded it (.issues has no such key, structurally), and
# the issue's own events carry the daemon's exact no-repository admission refusal
# (internal/workflow/repository.go:76, the resolvesToNone case), word for word.
negative_no_repo() {
  local key=$1 state has events ok=1
  state=$(state_json)
  has=$(jq -r --arg k "$key" '.issues | has($k)' <<<"$state")
  [ "$has" = false ] || ok=0
  events=$(dispatch_events "$key")
  local bodies
  bodies=$(jq -r '[.[] | select(.type=="message.created") | .payload.body] | join("\n---\n")' <<<"$events")
  grep -qF "Legion did not admit this issue: its repository resolves to none. Set its repository (the issue header's Repository picker, or dispatch issue-update --repo owner/name) and move it to todo again." <<<"$bodies" || ok=0
  evidence "negative-no-repo-$key" "issues has($key): $has
---
$bodies
" >/dev/null
  check "negative-no-repo-$key" "$ok"
}

# negative repo-edit before|after <run id>? <child KEY>: the baseline is the daemon's own recorded
# repository (unaffected by the edit) before it. `after` PASSes only when (1) the daemon's record
# is unchanged from the baseline, (2) Dispatch's own `.repo` field for the issue now reads X,
# confirming the edit really happened there, (3) an issue.updated event postdates the baseline
# (Dispatch's own structured proof of when the edit landed), and (4) the issue's events carry the
# daemon's exact kept-repository message (internal/workflow/repository.go:94), word for word with
# the baseline's repository and X substituted in.
negative_repo_edit() {
  local phase=$1
  if [ "$phase" = before ]; then
    local child=$2
    local repo
    repo=$(negative_issue_repo_of repo-edit "$child")
    local snap
    snap=$(jq -n --arg repo "$repo" '{daemon_repo: $repo}')
    baseline_capture repo-edit "$child" "$snap"
    return
  fi
  local run_id=$2 child=$3
  local baseline b_started b_repo
  baseline=$(baseline_verify repo-edit "$child" "$run_id")
  b_started=$(jq -r '.started_at' <<<"$baseline")
  b_repo=$(jq -r '.daemon_repo' <<<"$baseline")
  local state now_repo ok=1
  state=$(state_json)
  now_repo=$(jq -r --arg k "$child" '.issues[$k].repo // "none"' <<<"$state")
  [ "$(lower "$now_repo")" = "$(lower "$b_repo")" ] || ok=0
  local detail dispatch_repo_now
  detail=$(dispatch_issue "$child")
  dispatch_repo_now=$(jq -r '.repo // "none"' <<<"$detail")
  [ "$(lower "$dispatch_repo_now")" = "$repo_x" ] || ok=0
  local events edit_after bodies
  events=$(dispatch_events "$child")
  edit_after=$(jq --arg since "$b_started" '[.[] | select(.type=="issue.updated" and .created_at > $since)] | length' <<<"$events")
  { [ -n "${edit_after:-}" ] && [ "$edit_after" -gt 0 ]; } 2>/dev/null || ok=0
  bodies=$(jq -r '[.[] | select(.type=="message.created") | .payload.body] | join("\n---\n")' <<<"$events")
  grep -qF "Legion keeps $b_repo as this issue's repository; the change to $LEGION_E2E_REPO_X in Dispatch is not applied" <<<"$bodies" || ok=0
  evidence "negative-repo-edit-$child-$run_id" "baseline: repo=$b_repo started_at=$b_started
now: daemon repo=$now_repo, Dispatch repo=$dispatch_repo_now
issue.updated events after baseline: $edit_after
---
$bodies
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
