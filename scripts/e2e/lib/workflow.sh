# shellcheck shell=bash disable=SC2154,SC2034 # the variables named below are the caller's to set or read
# The Go workflow proof's vocabulary, shared by the stage proofs that drive a Dispatch issue through
# the Go daemon's workflow with real agents: Dispatch, the daemon's state, each phase's worker, the
# handoffs the daemon accepted, the smoke repository's pull request under the proof human, and the
# negative controls. A stage proof sources lib/rig.sh first.
#
# Sourced, never run. The caller sets
#   work           the run's scratch directory, holding dispatch-token, legion.yaml and operator-token
#   evidence       the evidence directory
#   project        the Dispatch project the run's issues live in; the daemon writes as
#                  legion-daemon:<project>
#   repo           the smoke repository, owner/name
#   port_dispatch  the Dispatch server's port on 127.0.0.1
#   port_daemon    the daemon's port on 127.0.0.1
#   pg_container   the container holding the daemon's Postgres database
#   pr_number      the issue's pull request, once it exists
#   smoke_file     the one product file that pull request's first implementation changed: the
#                  spec's one-file change, which every review round's correction appends to
# and defines note, pass and fail (which exits), plus the runtime seam — the only reads that depend
# on where an agent runs:
#   assert_claim_endpoints ISSUE ROLE   fails the check when the claim's process could reach a
#                                       service outside the rig; runs before every instruction
#   claim_session_text ISSUE ROLE       prints the claim's session file; fails when there is none
#   workspace_jj ISSUE ARGS...          runs jj ARGS in the issue's workspace
# drive_gate sets gate_artifact and gate_version.

# ---- Dispatch -------------------------------------------------------------------------------------

dispatch_url() { printf 'http://127.0.0.1:%s' "$port_dispatch"; }
dispatch_get() {
  curl -fsS --max-time 20 -H "Authorization: Bearer $(cat "$work/dispatch-token")" "$(dispatch_url)/api/v1/$1"
}
# dispatch_events ISSUE prints the issue's whole event log, paging past Dispatch's 200-event limit.
dispatch_events() {
  local issue=$1 after=0 page all='[]'
  while :; do
    page=$(dispatch_get "issues/$issue/events?after=$after&limit=200")
    all=$(jq -c --argjson page "$page" '. + $page' <<<"$all")
    [ "$(jq length <<<"$page")" -eq 200 ] || break
    after=$(jq '.[-1].seq' <<<"$page")
  done
  printf '%s\n' "$all"
}
dispatch_status_is() { dispatch_get "issues/$1" | jq -e --arg status "$2" '.status == $status'; }
review_cap_posted() {
  dispatch_events "$1" | jq -e 'any(.[]; .type == "message.created" and (.payload.body | contains("review_round_cap=3")))'
}
dispatch_human() {
  local method=$1 path=$2 body=${3:-}
  if [ -n "$body" ]; then
    curl -fsS --max-time 20 -X "$method" -H 'X-Dispatch-User: smoke' -H 'content-type: application/json' \
      --data "$body" "$(dispatch_url)/api/v1/$path"
  else
    curl -fsS --max-time 20 -X "$method" -H 'X-Dispatch-User: smoke' "$(dispatch_url)/api/v1/$path"
  fi
}
new_issue() {
  local title=$1 parent=${2:-} payload
  payload=$(jq -cn --arg project "$project" --arg title "$title" --arg parent "$parent" \
    'if $parent == "" then {project:$project,title:$title} else {project:$project,title:$title,parent:$parent} end')
  dispatch_human POST issues "$payload" | jq -er .key
}
set_status() { dispatch_human PATCH "issues/$1" "$(jq -cn --arg status "$2" '{status:$status}')" >/dev/null; }

# ---- the daemon's state ---------------------------------------------------------------------------

daemon_state() { "$work/legion" state --json --port "$port_daemon"; }
state_file() { daemon_state >"$evidence/$1.json"; }
issue_phase() { daemon_state | jq -e --arg issue "$1" --arg phase "$2" '.issues[$issue].phase == $phase'; }
issue_phase_in() {
  local issue=$1
  shift
  daemon_state | jq -e --arg issue "$issue" '.issues[$issue].phase as $p | $ARGS.positional | index($p) != null' --args "$@"
}
issue_worker_state() {
  daemon_state | jq -e --arg issue "$1" --arg role "$2" --arg state "$3" \
    'if $role == "architect" then .issues[$issue].architect.state == $state else .issues[$issue].workers[$role].claim.state == $state end'
}
issue_worker_session() {
  daemon_state | jq -er --arg issue "$1" --arg role "$2" '.issues[$issue].workers[$role].claim.session'
}
architect_session() { daemon_state | jq -er --arg issue "$1" '.issues[$issue].architect.session'; }
issue_worker_live() {
  local issue=$1 role=$2 not_pane=${3:-}
  daemon_state | jq -e --arg issue "$issue" --arg role "$role" --arg not_pane "$not_pane" '
    (if $role == "architect" then .issues[$issue].architect else .issues[$issue].workers[$role].claim end) as $claim
    | ($claim.session // "") != "" and ($claim.state | IN("ready", "working", "idle"))
      and ($not_pane == "" or ($claim.locator.tmux.pane // "") != $not_pane)'
}
# tree_suspended ISSUE: the lingering tree's root architect is suspended, its session kept.
tree_suspended() { issue_worker_state "$1" architect suspended; }
claim_token() { printf 'legion-%s-%s-%s' "${project,,}" "${1,,}" "$2"; }
claim_incarnation() {
  daemon_state | jq -er --arg issue "$1" --arg role "$2" '.issues[$issue].workers[$role].claim.locator.incarnation // empty'
}
# launches_after_failure CLAIM counts the daemon's launches of CLAIM logged after it failed it.
launches_after_failure() {
  jq -R -s --arg claim "$1" '
    [split("\n")[] | fromjson? | select(.claim == $claim)] as $lines
    | ([$lines | to_entries[] | select(.value.msg == "supervise: claim failed") | .key] | last) as $failed
    | if $failed == null then -1 else [$lines[($failed + 1):][] | select(.msg == "supervise: launched")] | length end
  ' "$evidence/logs/daemon.log"
}
db_value() { docker exec "$pg_container" psql -U legion -d legion -tAc "$1"; }

# ---- driving the workflow -------------------------------------------------------------------------

# A role's ordinary state is enough for the protocol; each real agent receives a deliberately
# narrow smoke instruction so the proof observes the workflow rather than an arbitrary feature.
# wait_for_phase ISSUE PHASE [SECONDS] waits for ISSUE to reach PHASE, 600 s unless SECONDS says:
# a phase whose worker runs its whole loop (a correction round, the retro) takes longer.
wait_for_phase() { until_true "${3:-600}" "$1 to reach $2" issue_phase "$1" "$2"; }
wait_for_worker() {
  until_true 300 "$2 worker on $1 to register" issue_worker_live "$1" "$2"
  assert_claim_endpoints "$1" "$2"
}
# A targeted human Dispatch message is the proof operator's only instruction surface for an agent.
# Each message is delivered through the same listener and plugin the product uses.
send_agent() {
  local issue=$1 role=$2 message=$3 session body
  if [ "$role" = architect ]; then session=$(architect_session "$issue"); else session=$(issue_worker_session "$issue" "$role"); fi
  assert_claim_endpoints "$issue" "$role"
  body=$(jq -cn --arg body "$message" --arg target "session:$session" '{body:$body,target:$target,delivery:"steer"}')
  dispatch_human POST "issues/$issue/messages" "$body" >/dev/null
  note "sent $role instruction to session $session"
}
# A claim's session file is the persisted evidence of what that one real agent saw and ran. The
# lookup is exact to its claim: another agent's transcript can never satisfy the check.
# The text is captured before grep reads it: piped, grep's first match closes the pipe while the
# writer still has a long session to send, and under pipefail the SIGPIPE (141) reads as absent.
session_contains() {
  local text
  text=$(claim_session_text "$1" "$2") || return 1
  grep -Fq -- "$3" <<<"$text"
}

# The architect owns spec editing and gate registration; the proof names the one primary artifact
# Dispatch created so a real agent cannot register an unrelated document.
drive_gate() {
  local issue=$1 label=$2 artifact
  artifact=$(dispatch_get "issues/$issue" | jq -er .primary_artifact_id)
  wait_for_worker "$issue" architect
  send_agent "$issue" architect "$label: update this issue's primary spec document with one tiny, concrete one-file smoke change for $repo, and say in it that a review of the pull request may ask for one more line appended to that same file, which is in scope. Request approval for primary artifact $artifact. Then use the Go-daemon Legion operation to register the gate for exactly artifact $artifact at the version returned by that approval request. Wait after registering."
  until_true 300 "$label architect to register primary artifact $artifact" sh -c \
    "'$work/legion' state --json --port '$port_daemon' | jq -e --arg issue '$issue' --arg artifact '$artifact' '.issues[\$issue].designGate.artifactId == \$artifact and .issues[\$issue].designGate.currentVersion > 0'"
  gate_artifact=$(daemon_state | jq -er --arg issue "$issue" '.issues[$issue].designGate.artifactId')
  gate_version=$(daemon_state | jq -er --arg issue "$issue" '.issues[$issue].designGate.currentVersion')
  dispatch_human POST "artifacts/$gate_artifact/reviews" '{"state":"approved"}' >/dev/null
  wait_for_phase "$issue" planning
  wait_for_worker "$issue" planner
}

# ---- the proof human on the smoke repository ------------------------------------------------------

# pull_request_product_files prints each file the pull request changes outside .legion/.
pull_request_product_files() {
  gh api --paginate "repos/$repo/pulls/$pr_number/files" --jq '.[] | select(.filename | startswith(".legion/") | not) | .filename'
}
# A direct review is deliberately the devbox's ordinary gh acting as the proof human. It is never
# `legion gh`, and the bridge is the only path that carries the event to the daemon.
request_changes() {
  local body=$1
  gh -R "$repo" pr review "$pr_number" --request-changes --body "$body"
}
# round_line ROUND is the line a scripted review round asks for: distinct per round and run, and
# within the spec, whose architect was told a review may ask for one more line in the smoke file.
round_line() { printf 'Stage 3 review round %s (%s)' "$1" "$project"; }
# round_correction_pushed ROUND: that round's line is added to the smoke file, the one the round's
# review names; the line in any other file (a notes file, or a .legion/ handoff that quotes it) is
# not the correction.
# Each gh read below is captured before it is matched: piped, grep's first match would close the
# pipe while gh still writes, and under the caller's pipefail the SIGPIPE (141) would read as absent.
round_correction_pushed() {
  local patches
  patches=$(gh api --paginate "repos/$repo/pulls/$pr_number/files" --jq ".[] | select(.filename == \"$smoke_file\") | .patch // \"\"") || return 1
  grep -qF -- "+$(round_line "$1")" <<<"$patches"
}

# REST names the review App's account legion-reviewer[bot]; GraphQL (`gh pr view --json reviews`)
# drops the suffix, and a user could hold the bare name. The approval must be of the current head.
reviewer_approved_head() {
  local head approved
  head=$(gh api "repos/$repo/pulls/$pr_number" --jq .head.sha) || return 1
  approved=$(gh api --paginate "repos/$repo/pulls/$pr_number/reviews" \
    --jq '.[] | select(.user.login == "legion-reviewer[bot]" and .state == "APPROVED") | .commit_id') || return 1
  grep -qx -- "$head" <<<"$approved"
}
# The Go daemon has no clean-head loop yet: skills/legion-worker/SKILL.md wants APPROVE only for a
# head that carries no .legion/, then the implementer's .legion/ deletion push, and the Go workflow
# neither asks for that round nor waits for it. Its destination is Stage 7's clean-head loop. Until
# then the proof's reviewer approves the head it has, the merge carries the run's .legion/ handoffs
# and retro learnings onto the smoke main, and clean_smoke_main removes them after the merge.
approve_as_reviewer() {
  local issue=$1
  send_agent "$issue" reviewer "Stage 3 proof final review: use the bash tool to submit APPROVE on pull request #$pr_number in $repo at its current head as legion-reviewer[bot], then complete the reviewer handoff. This exact smoke instruction takes precedence over waiting for another review round."
  until_true 300 "legion-reviewer[bot] approval of pull request #$pr_number at its head" reviewer_approved_head
}
# smoke_main_leftovers prints each path on the smoke repository's main under .legion/ or
# docs/solutions/: the handoffs and retro learnings a merged proof pull request carries there.
smoke_main_leftovers() {
  gh api "repos/$repo/git/trees/main?recursive=1" \
    --jq '.tree[] | select(.type == "blob") | .path | select(startswith(".legion/") or startswith("docs/solutions/"))'
}
# clean_smoke_main removes every leftover from the smoke main through the proof human's ordinary
# merge (the smoke main takes changes only through pull requests), so the next run starts from a
# fixture whose base carries no other issue's handoff. See approve_as_reviewer for why a merge
# leaves them.
clean_smoke_main() {
  local paths base branch path sha url
  paths=$(smoke_main_leftovers)
  [ -n "$paths" ] || return 0
  base=$(gh api "repos/$repo/git/ref/heads/main" --jq .object.sha)
  branch="proof/clean-main-${project,,}"
  gh api "repos/$repo/git/refs" -f ref="refs/heads/$branch" -f sha="$base" >/dev/null
  while IFS= read -r path; do
    sha=$(gh api "repos/$repo/contents/$path?ref=$branch" --jq .sha)
    gh api -X DELETE "repos/$repo/contents/$path" -f message="proof fixture: remove $path" -f sha="$sha" -f branch="$branch" >/dev/null
  done <<<"$paths"
  url=$(gh -R "$repo" pr create --base main --head "$branch" --title "proof fixture: remove the handoffs and learnings Stage 3 runs merged ($project)" \
    --body "The Stage 3 proof run $project removes what merged proof pull requests left on main: .legion/ handoffs and docs/solutions/ retro learnings. The Go daemon has no clean-head loop before Stage 7, so each proof merge carries them. This is a proof fixture change by the proof's human-merge identity; it changes no product.")
  gh -R "$repo" pr merge "${url##*/}" --squash --delete-branch
  note "the proof human removed $(wc -l <<<"$paths") leftover paths from $repo main through $url"
}

# ---- the handoffs the daemon accepted -------------------------------------------------------------

# The record keeps each role's phase row: the commit carrying the handoff its completion reported,
# emptied when the role's next phase starts, and, on the implementer's row, the review round (its
# returns to implementing). The issue moves out of implementing, retro, and merging only on that
# phase's completion, and a production check's completion moves nothing, so a checker reads the
# row right after the transition it follows, before the role's next phase can empty it.

# role_handoff ISSUE ROLE [PHASE...] prints the commit carrying ROLE's accepted completion, when the
# issue stands in one of the PHASEs (any phase when none is named).
role_handoff() {
  local issue=$1 role=$2 phases='' p
  shift 2
  for p in "$@"; do phases="$phases${phases:+, }'$p'"; done
  db_value "select p.handoff_commit from phases p join issues i on i.key = p.issue
    where p.issue = '$issue' and p.role = '$role' and p.handoff_commit <> ''${phases:+ and i.phase in ($phases)}"
}
# review_round ISSUE prints the implementer's review round: 0 before its first return.
review_round() {
  db_value "select coalesce((select rounds from phases where issue = '$1' and role = 'implementer'), 0)"
}
# handoff_fact_commit ISSUE ROLE PHASE ROUND prints the commit carrying the handoff the daemon
# accepted for the role's completion of that phase round. The check runs right after the round's
# transition, before the role's next completion can move it.
handoff_fact_commit() {
  [ "$(review_round "$1")" = "$4" ] || return 0
  role_handoff "$1" "$2"
}
role_app() { case "$1" in implementer | merger) printf 'legion-implementer[bot]' ;; *) printf 'legion-reviewer[bot]' ;; esac; }
# assert_handoff_committer ISSUE ROLE PHASE ROUND: the commit carrying that completion's handoff is
# authored and committed by the role's own App, read from the issue's workspace (the commit need not
# be pushed), so no other pane sealed another role's handoff.
assert_handoff_committer() {
  local commit identity want
  commit=$(handoff_fact_commit "$1" "$2" "$3" "$4")
  [ -n "$commit" ] || fail "$1 has no $2 $3 round $4 handoff fact"
  identity=$(workspace_jj "$1" log -r "$commit" --no-graph -T 'author.name() ++ "|" ++ committer.name()' 2>&1) ||
    fail "read $1's $2 $3 round $4 handoff commit $commit: $identity"
  want="$(role_app "$2")|$(role_app "$2")"
  [ "$identity" = "$want" ] || fail "$1's $2 $3 round $4 handoff commit $commit is authored|committed by $identity, want $want"
  note "$2 $3 round $4 handoff $commit authored and committed by $identity"
}
# retro_reported ISSUE: the daemon applied the implementer's retro completion, the one way out of
# retro.
retro_reported() { [ -n "$(role_handoff "$1" implementer merging awaiting_merge)" ]; }
# production_check_reported ISSUE: the daemon applied the implementer's production-check completion
# (the completion moves no phase: the architect's sign-off does).
production_check_reported() { [ -n "$(role_handoff "$1" implementer production_check "done")" ]; }
# assert_round_handoff ISSUE ROUND: the issue reached testing on the implementer's own completion
# of that implementing round, never on a push alone carrying an earlier round's handoff.
assert_round_handoff() {
  if [ "$(review_round "$1")" != "$2" ] || [ -z "$(role_handoff "$1" implementer testing reviewing)" ]; then
    fail "$1 reached testing without the implementer's implementing round $2 handoff"
  fi
}

# ---- notices --------------------------------------------------------------------------------------

# A delivered notice is rendered into the receiving agent's session as the listener's envelope,
# `summary: <kind> on <issue>`, which no role prompt or proof instruction contains: the bare kind
# does appear in the architect's prompt, so it can never be the needle.
notice_needle() { printf 'summary: %s on %s' "$1" "$2"; }
# notice_deliveries ISSUE ROLE NEEDLE counts the Envoy deliveries in the claim's session holding
# NEEDLE: one session line per delivered message, and never a line the agent wrote itself. The
# outbox cannot count them: the runner deletes each row it finishes.
notice_deliveries() {
  { claim_session_text "$1" "$2" || true; } | grep -F '"customType":"envoy-message"' | grep -cF -- "$3" || true
}
notice_delivered() { [ "$(notice_deliveries "$@")" -ge 1 ]; }

# ---- negative controls ----------------------------------------------------------------------------

# The negative controls mutate only captured evidence, never the production-like rig. Each
# checker is the exact assertion the positive check uses; a corrupt copy must be rejected before
# the original is accepted again.
expect_failure() {
  local name=$1
  shift
  if "$@" >"$evidence/negative-$name.out" 2>&1; then
    fail "negative control $name unexpectedly passed"
  fi
  note "negative control $name rejected the deliberately broken observation"
}
# An issue event's payload is the whole issue after the write, so a status write is an event whose
# status differs from the issue event before it (`issue.closed` for done). Every lifecycle
# transition must carry the daemon's actor, and all five lifecycle statuses must appear, so the
# check cannot pass on a history the workflow never wrote.
assert_status_actors() {
  local file=$1
  jq -e --arg daemon "legion-daemon:$project" '
    [ .[] | select(.type | IN("issue.created", "issue.updated", "issue.closed")) ] | sort_by(.seq)
    | [ range(1; length) as $i | select(.[$i].payload.status != .[$i - 1].payload.status) | .[$i] ]
    | map(select(.payload.status | IN("in_progress", "testing", "needs_review", "retro", "done")))
    | (map(.payload.status) | unique) == ["done", "in_progress", "needs_review", "retro", "testing"]
      and all(.actor.id == $daemon)
  ' "$file" >/dev/null
}
