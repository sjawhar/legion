---
title: "A live proof of a new workflow order fails on its own rig first: six reads that held only while the old order ran"
category: testing
tags:
  - stage4b
  - e2e-rig
  - pipefail
  - sigpipe
  - jq
  - until_true
  - node-release
  - secret-leaks
  - ISSUE_CLOSED
  - durable-consumers
date: 2026-10-21
status: active
module: scripts/e2e
related_issues:
  - "LEGION-781"
  - "sjawhar/legion#2059"
---

# A live proof of a new workflow order fails on its own rig first: six reads that held only while the old order ran

A daemon change that moves where an issue closes, what runs after, and how long a pod lives
changes what the proof's own reads see. Expect the first runs of the stage to fail on the rig,
not the daemon, and read each failure's artifact for which it is before touching runtime code.
These six held at main and broke under the new order.

- **A fixture for work after the close waits for no Dispatch message.** When this was written a
  closed issue took no Dispatch write (`409 ISSUE_CLOSED`; since INFRA-448 it takes comments and
  messages, and still refuses asks and document edits), so a `send_agent` to the implementer
  of a just-closed issue failed the checkpoint, and an instructions sentence telling that agent to
  "reply WAITING and wait for the targeted message" holds its look forever. The daemon's own task
  text carries the look's rule; the proof reads the report from the daemon's state.
- **A shared resource is judged against every co-tenant it can legitimately hold.** A node's
  "released" read classifies each pod: tree 1's own and the run's with no tree label hold it; the
  run's repository cache (`legion.dev/cache=true`, alive as long as the daemon), another tree of
  the run (a carried child's, launched at the linger's expiry — now inside the wait) and another
  project keep it; a pod with no project label counts for nothing. Name the category in the note.
  Read tree 1's linger `until` from the daemon's line and assert its Sandboxes only while it runs.
- **A leak scanner excludes keys by the producer's own constants, never by value shape.** A role
  Secret's `github-default` (the owner) and `github-config-<owner>` are plaintext by design and
  match every pod's `<owner>/<repo>` args; exclude exactly those names (`names.go`
  `GitHubDefaultKey`, `GitHubConfigKey`), judge every other key, and print `judged`/`excluded`
  in the verdict so a new key's omission is visible.
- **A `kubectl exec | grep -q` under pipefail reads a running command as absent** once the match
  falls early in a long list (141). Capture, then judge the variable
  (docs/solutions/testing/a-kustomize-render-is-a-copy-of-every-secret-test-it-in-memory-and-prove-absence-by-value.md
  states the rule); the delta here: a *negative* read (`! pod_runs …`) must capture too, and fail
  by name when the list cannot be read, or an unreadable list passes as "stopped".
- **A new jq predicate runs once by hand over a real listing before it guards a wait.**
  `until_true` drops the predicate's stderr, so `jq: error: Cannot index string with "role"` (a
  `.role` read inside `IN(...)`, where the input is already the state string) is a 300 s timeout
  with the daemon standing exactly where the predicate looks. Bind the allowed set
  (`(if .role == … then $i else $o end) as $allowed`) before piping the field into `IN($allowed[])`.
- **A teardown deletes a project's durables by listing them, never by naming the two it knew.**
  A daemon that opens a third consumer leaves one behind, and hygiene then fails the *next* run.

## Evidence

Seven Stage 4b runs of sjawhar/legion#2059 (LEGION-781: `accepting` before the merge, the
production check after the close), each failing one checkpoint past the last:

- 4ab26ee5: `done` timed out 300 s while the daemon log showed six linger suspends, look 1's start
  row resuming the implementer and its task in-session; `claim_states_are`'s jq exited 5 on every
  listing (`scripts/e2e/lib/workflow.sh`, the tester's `claim-states.test.ts` red then green).
- 22817ba1: `done` reached the close (LEGSMOKE-836 `awaiting_merge → done`, the implementer
  resumed with the check task) and failed on `send_agent … 409 ISSUE_CLOSED`; the try-2 transcript
  shows the implementer's `WAITING` reply to the fixture's own sentence (23:05:32Z), not the daemon
  holding the look. Both post-close sends in Stage 4b and both in Stage 3 removed; the fixtures'
  instructions restated.
- a578760e: `node-release` timed out 1500 s on node ip-10-1-11-161 holding the run's cache pod
  (since 06:02Z) and LEGSMOKE-860's new-tree pod (created 09:32:29Z, three seconds after tree 1's
  linger expired inside the wait); `node_pods_judged` and `lib/node-released.test.ts`.
- 59e0337c: `mid-turn-carry` read the planner's running `/usr/bin/sleep 13` as absent on all 51
  polls — `mid-turn-hold-lost.txt` has it at byte 16556 of 36847; the tester's `pod-runs.test.ts`
  red at 141, green after the capture.
- 90a60026: `pod-shape` flagged 114 "leaks" over 19 pods, every one the owner name;
  `lib/secret-leaks.ts` and the tester's `secret-leaks.test.ts`.
- Round 4 (e4d28dc2): `hygiene` found `legion-go-<project>-dispatch-server` left after a teardown
  that deleted `-dispatch` and `-github` by name.
- ab4eccee: `stage 4b e2e: PASS`, 55 of 56 (one skipped by design), the daemon side having stood
  where the predicates looked since 4ab26ee5
  (`dispatch://LEGION-781/artifact/stage4b-2059-ab4eccee-tgz`).
