# INFRA-251 round 15: kept decisions keyed by issue — the state table

A person's decision made where nobody in its tree can hear it is kept in `parked_decisions`, keyed
by `(issue, ref)`: the issue it was decided on, never a root. The tree that owes the person an
answer is the tree that issue stands in **now**, read from the record every time a start task or a
catch-up names it, tells it, or is narrowed before it is sent. An issue that changes trees — a
child a linger hands to Legion with its run or without it, a parked child a person's todo orphans,
a grandchild handed over beside its parent, a queued root admission adopts into a live tree — takes
what was decided on it with it, in any order, with nothing to move.

`readmitted_approvals` is keyed the same way, by the issue whose spec it is (the root), and carries
the same claim and hold columns, so it travels through the same four states.

## The states

| State | How the row stands | Who may name it | How it leaves |
| --- | --- | --- | --- |
| `KEPT` | `claimed_by is null`, `held_by_claim is null` | the next start or catch-up of its issue's tree | a naming claims it |
| `CLAIMED(r)` | `claimed_by = r`, no hold | nobody, while the claim counts | the catch-up `r` publishes (TOLD), the claim's write of the start's delivery (HELD), `r`'s hand-over or deletion (KEPT) |
| `HELD(c,d)` | `held_by_claim = c`, `held_by_delivery = d`, `claimed_by is null` | nobody, while the hold counts | the confirmation of delivery `d` (TOLD), its retirement or a send-time drop (KEPT), its take-back under a new id (HELD(c,d′)) |
| `TOLD` | the row is deleted | — | — |

`KEPT` is also where a row starts: `AddParkedDecision` writes it, and a newer decision on the same
reference replaces the row and clears its claim and hold, so the next namer names the newest
decision and whatever named the older one leaves it alone.

## The transitions

| From | To | Where | What happens |
| --- | --- | --- | --- |
| — | KEPT | `record.RelayParkedDecision` → `AddParkedDecision` | a person decides where nobody in the tree hears it; the controller gets its notice |
| KEPT | CLAIMED(r) | `workflow.EnqueueNaming` → `Store.ClaimKeptDecisions`, in the transaction that enqueues `r` | a root-architect start or a catch-up of the row's tree names it |
| CLAIMED(r) | HELD(c,d) | the claim store's `putDelivery`, in the transaction that writes the delivery | the architect's claim `c` queues the start's task as delivery `d` |
| CLAIMED(r) | TOLD | `Store.ForgetKeptDecisions`, after the catch-up of `r` published and marked `kept-told` | the catch-up told it, and the row's issue still stands in the teller's tree |
| CLAIMED(r) | KEPT | `Store.ForgetKeptDecisions` (the issue left the teller's tree), `workflow.HandOverKept` → `ReleaseKeptClaims` (the row reached no architect), or `r`'s deletion (`on delete set null`) | nothing told it, or what told it was not the architect it is owed to |
| HELD(c,d) | TOLD | the claim store's `tellKeptDecisions`, in the transaction that writes `confirmed_at` | the turn carrying that delivery started, and the row's issue still stands in `c`'s tree |
| HELD(c,d) | KEPT | the same transaction (the issue left `c`'s tree); `releaseKeptDecisions` at `RetireDelivery`; `Store.ReleaseHeldDecisions` at the send-time drop | the delivery told it to nobody it was owed to, or ended without telling |
| HELD(c,d) | HELD(c,d′) | the claim store's `putDelivery` | a death took the task back under a new id (`interrupted`, `takeBackPending`) |

Two transitions write a fresh catch-up as well, so that nothing waits KEPT while the tree it is
owed to has an architect to hear it: `HandOverKept` writes one to the architect of **each** tree
the rows it released are owed to now (`workflow.CatchUpTheTreesOwed`, for the launch that tree's
architect claim stands at), and so does the send-time drop in the daemon's `rootTask`.

## The validity rule, on both sides

One rule governs naming and telling, and it is read from the record at the moment of each:

- **Naming for tree T** (`Store.UnclaimedKept`): a row may be named iff its issue stands in T now
  **and** it carries no *counting* claim and no *counting* hold.
  - `claimed_by = r` counts iff the tree of outbox row `r`'s issue is the row's issue's tree.
  - `held_by = (c, d)` counts iff claim `c`'s tree is the row's issue's tree **and** `c` still holds
    delivery `d` unconfirmed.
  - A void claim or hold is ignored. That is the whole fix: at a hand-over, an orphaning, an
    adoption or a grandchild's hand-over, in any order, the rows an issue takes with it carry
    claims and holds of the tree it left, which count for nothing in the tree it joined.
- **Telling** (the architect's hold A): a catch-up's publish (`Store.ForgetKeptDecisions`) and a
  held delivery's confirmation (`tellKeptDecisions`) delete a row only if, **in that same
  transaction**, its issue still stands in the teller's tree — the catch-up row's issue's tree, or
  the claim's tree. Otherwise the row goes back to KEPT, claim and hold cleared, for its own tree
  to name. The old architect also hearing it is the lesser fault.
- **Hold B** (`supervise`'s `turnStarted`): a held root-architect task of a run is confirmed only
  by its own turn — a send of the machine's own, or a prompt it waits for a turn on — never by the
  question a restart left (`askFirst`) or a foreign turn while the root does not hear. The
  confirmation is the telling, so a turn that is not this task's must not make it.

## Where the CLAIMED → HELD move is written, and why it is not the settlement

The design said the start's settlement moves claim `r` to the hold. It is written in the claim
store's delivery write instead (`internal/store/claims.go`, `putDelivery`), which the architect
accepted. Everything else is unchanged: the states, the rule, and the fact that a held row is told
at its own delivery's confirmation and never at queue.

The reason is atomicity. The confirmation must tell what the delivery holds in the transaction that
writes `confirmed_at`, and the retirement must leave it KEPT in the transaction that deletes the
delivery. If the hold were written later, by the settlement that runs after the row's work, a
confirmation landing in between would tell nothing (there is no hold to read), the settlement would
then hold a confirmed delivery, and the retirement at the turn's end would return the rows to KEPT —
telling the same architect a second time, which the invariant forbids. Writing the hold with the
delivery closes that window: a row is never held by a delivery the store does not hold, and a
delivery never records a confirmation without telling what it held.

What the settlement still does (`daemon.outbox.settleKept`) is the other half: a start whose task
reached **no** claim — gave nothing and is not in `Claim.DeliveredRows` — hands what it claimed over
(`workflow.HandOverKept`). A start whose task the claim took has nothing left to settle.

One consequence: `supervise.Store` implementations must do this, so an in-memory fake bypasses it.
The tests that exercise a kept decision through a start therefore run against the daemon's own claim
store.

## How claims, holds and DeliveredRows read under issue keys

- **`claims.delivered_rows` is unchanged.** It is still every outbox start row whose task this claim
  queued, and still what says, when a start settles, whether the claim took its task. It says
  nothing about which rows a decision stands in: that is the hold.
- **The hold is `(claim token, delivery id)`**, not a row id, because the delivery is what outlives
  the start row and what the architect is eventually prompted with. A delivery taken back under a
  new id carries its holds to that id, so the hold always names a delivery the store holds.
- **A held task names only what it still holds.** At send time (`Deps.RootTask`, the daemon's
  `rootTask`, in one serialized write):
  1. marks whose reference this delivery no longer holds (`Store.HeldKeptRefs`) are dropped in
     silence — the row was told by a catch-up that reached the architect first, replaced by a newer
     decision on the same reference, or released by an earlier send;
  2. marks whose issue has left the claim's tree are dropped, their hold released
     (`Store.ReleaseHeldDecisions`), and the architect of each tree they are owed to now is written
     a fresh catch-up naming them;
  3. what is left is the task, written again from those marks (`workflow.KeptDecisionsTask`) under
     the same delivery id, and persisted before it is sent;
  4. a task left naming nothing is retired rather than sent, and the retirement's own write leaves
     whatever it still held KEPT.
- **The close's owed list for tree T** (`workflow`'s `warnKeptDecisionsOfAClosedTree`) is every row
  whose issue stands in T now, in any state — KEPT, CLAIMED or HELD — read by `Store.ParkedDecisions`
  in one query. A decision on a member the same close hands to Legion is logged as handed over, not
  warned about; one on a member the close leaves behind (already out of the workflow, below a handed
  child or not) is a Warn line, because that member stays in the closing tree and its row stays with
  it. There is no subtree walk left: `linger.handedOverSubtrees` is gone, as are
  `Store.MoveKeptDecisions`, `ReKeepDeliveredDecisions` and `HeldKeptDecisions`, and the calls
  `admit.Admission.readmit` made to the first two.
