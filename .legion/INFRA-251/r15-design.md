# INFRA-251 round 15: kept decisions keyed by issue

## Workspace and environment
Workspace: the absolute path in `$LEGION_WORKSPACE` (sjawhar/legion, jj not git). Go module `packages/daemon`.
Large files only under `$TMPDIR` (`/legion/scratch/tmp`). No commit to legion/INFRA-251, no GitHub
write. The pod can be replaced without warning, losing unpushed work: every ~30 minutes, and before
any run longer than 10 minutes, save work in progress with exactly:

```
cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" describe -m "wip(daemon): INFRA-251 round 15 in progress" && jj -R "$LEGION_WORKSPACE" bookmark set legion/INFRA-251-wip -r @ && jj -R "$LEGION_WORKSPACE" git push -b legion/INFRA-251-wip
```
(no other jj command). Say in your report each time you pushed.

Every Go test runs from `packages/daemon` with this prefix (PostgreSQL 17.2 on 127.0.0.1:55432, NATS on
54222 are running; do not stop them):
```
env -u LEGION_WORKSPACE -u LEGION_WORKSPACE_RECREATED -u OMP_SESSION_SQL_DSN_FILE -u OMP_SESSION_STORAGE \
  LEGION_TEST_PG_DSN='postgres://postgres:pw@127.0.0.1:55432/postgres?sslmode=disable' \
  LEGION_TEST_NATS_URL=nats://127.0.0.1:54222 LEGION_TEST_NATS_MONITOR_URL=http://127.0.0.1:58222 go test ...
```
Every run is bounded with `timeout`; property runs use fixed LEGION_KEPT_PROPERTY_RUNS/STEPS. No
open-ended loops, no `mapfile < <(...)`.

## The machinery today (read the code)
A person's decision on a tree nobody in it can hear is kept in `parked_decisions` (migration
`0065_parked_decisions`, today keyed (root, ref) with an `issue` column) until the root architect's
start task or catch-up that names it reaches the architect. Each row is claimed by one outbox row
(`claimed_by`). Code: `internal/record/parked_decision.go` (RelayParkedDecision), `internal/record/store.go`
(kept-decision methods, DropCatchUps, MoveKeptDecisions, ReKeepDeliveredDecisions, HeldKeptDecisions),
`internal/workflow/parked_decision.go` (ParkedDecisionTask, EnqueueNaming, HandOverKept, ArchitectHears,
KeptDecisionsTask), `internal/workflow/catch_up.go` (QueueCatchUp), `internal/workflow/linger.go`
(warnKeptDecisionsOfAClosedTree, handedOverSubtrees), `internal/daemon/parked_decision.go` (settleKept,
handOverKept, toldKept, settleTold, supersededKept), `internal/daemon/outbox.go`,
`internal/daemon/notice_exceptions.go`, `internal/daemon/workflow.go` (rootHears, rootTask),
`internal/supervise/delivery.go` + `table.go` (DeliveredRows, RootHears, RootTask, asksRoot, held tasks,
askFirst), `internal/admit/admit.go` (readmit's orphan branch calls MoveKeptDecisions and
ReKeepDeliveredDecisions), migration `0066_claim_delivered_rows` (claims.delivered_rows,
pending_task_deliveries.kept_decisions). Main's migrations end at 0064; 0065 and 0066 are this branch's
and may be edited in place.

## Review 5720641456's three losses (all: an issue changes trees, its rows stay under the old root)
1. admit.go:866: a child that leaves its tree without its run (a blocked Admitted child handed over;
   a person's todo orphaning a parked child) keeps its rows under the old root; the close calls it fine.
2. A queued root (WaitsForSlot) keeps a decision under its key, then is adopted into live tree T
   (workflow/adopt.go Adopt); nothing reads the row again.
3. admit.go:989: a grandchild handed over beside its parent loses its rows to the parent, by order.

Red tests (commit bb7e3e4c, must pass unmodified): internal/admit/lost_kept_decisions_test.go (4 tests),
the extended property model TestKeptDecisionsReachTheCurrentArchitectOnce (oracle: a decision is owed to
the architect of the tree its issue stands in, moving with the issue while kept or held),
TestKeptDecisionsRegressionSeeds, and round 13's TestADecisionHeldInTheOldArchitectsTaskAtAHandOverReachesTheChild.

## The design (agreed with the architect)
Remove the cause: key each kept decision by the issue it is about, never by a root; resolve the tree
and architect told from the record when a start or catch-up names it.

Schema (0065/0066 in place): parked_decisions primary key (issue, ref), issue references issues(key)
on delete cascade; no root column; claimed_by bigint references outbox(id) on delete set null;
held_by_claim text and held_by_delivery text (claim token and delivery id of the queued start task
that names it). readmitted_approvals keyed by issue (the root whose spec it is), same claim/hold columns.

States: KEPT (no claim, no hold); CLAIMED(r) (outbox row r, a root-architect start or a catch-up, names
it); HELD(c,d) (the start's task queued on claim c as delivery d; the start's settlement moves claim r to
the hold, where the forget is today); TOLD (deleted).

One validity rule, on both sides:
- Naming for tree T: rows whose issue is in T now and that have no counting claim or hold. claimed_by=r
  counts iff outbox row r's issue's tree is the row's issue's tree; held_by=(c,d) counts iff claim c's
  tree is the row's issue's tree. A void claim or hold is ignored, so nothing moves at a hand-over, an
  orphaning, an adoption or a grandchild's hand-over, in any order.
- Telling (architect's hold A): a catch-up's publish, or a held delivery's confirmation, marks a row
  TOLD only if in that same transaction the row's issue still stands in the teller's tree (the catch-up
  row's issue's tree / the claim's tree); otherwise the row goes back to KEPT (claim/hold cleared) for
  its issue's tree to name; the old architect also hearing it is the lesser fault.
- Held rows are told at their own delivery's confirmation (the store write recording ConfirmedAt deletes
  the rows held by that claim and delivery id, under the rule above, same transaction), never at queue.
- Hold B: round 13's guard stays: a held root task is confirmed only by its own turn, never by askFirst
  or a foreign turn while the root does not hear.
- RootTask at send time: drop marks whose issue left the claim's tree, clear those rows' hold (KEPT),
  and if that issue's tree's root architect hears, queue it a catch-up (HandOverKept's way); a task
  left naming nothing is retired. Retire clears a delivery's holds; an interrupted task taken back under
  a new id moves its holds to the new id. DeliveredRows unchanged. A start whose task reached no claim
  hands over (HandOverKept) by the issue's tree.
- The close's owed list for T: rows whose issue is in T now (any state).

Delete: MoveKeptDecisions, ReKeepDeliveredDecisions (and HeldKeptDecisions if unused) and their calls
in readmit, linger.handedOverSubtrees. RelayParkedDecision keeps under the decision's issue (the root is
still read for WaitsForSlot and the controller notice).

The rule: a person's decision is never lost; telling it twice is the lesser fault.
