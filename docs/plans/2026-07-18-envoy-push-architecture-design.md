# Envoy Push Architecture + Architect Ownership — Design Spec

> Status: v3 — routing/delivery layer corrected after hyperplan adversarial round (5 members, 3 rounds) + Sami decisions. Recursion/architect-ownership sections unchanged from v2. Execution plan: see §14.
> Date: 2026-07-19
> This is the **decision record**. Execution splits into separate implementation plans (§14).

## 1. Goals

1. **Kill polling.** All inter-agent and system→agent communication is push, carried by Envoy. The controller is woken only when action is needed; workers are woken directly by events they own.
2. **Recursion with ownership.** Architects decompose issues into native GitHub sub-issues; every issue gets its own architect; the architect owns its issue through delivery, with final approval (veto) before merge.
3. **Converge on Go without a big-bang migration.** New push infrastructure is Go from day one; the TS daemon shrinks by deletion, not porting.
4. **Overhaul the backlog/roadmap** around these decisions.

## 2. Verified ground truth

Facts the design is built on — each verified against code or official GitHub docs:

**GitHub (verified against docs.github.com):**
- Sub-issues API + dedicated `sub_issues` webhook event exist; max depth 8; `Issues: write` suffices. **Cycle rejection is NOT documented** — empirical test required (§15).
- `opened` vs `ready_for_review` are distinct `pull_request` actions — draft-as-quiet works natively.
- Re-requesting review from a prior reviewer works and re-fires `review_requested`. **Same-identity self-request and self-approve are hard-blocked at the API.** Two GitHub Apps exist (implementer, reviewer) — covers native review flow.
- **Check runs have no author-identity restriction** and can be required status checks. `Checks: write` requires **per-installation re-approval**. Branch protection can **pin one source App per required check name**. GitHub warns identical check names colliding with Actions job names produce ambiguous required checks.
- Creating a check run fires a `check_run` webhook the creating App also receives — consumers filter by `check_run.app.id` + name + head SHA.
- A new push (new head SHA) resets required checks to `expected` — **native head-SHA fencing**.
- GitHub never auto-redelivers failed webhooks; deliveries API (list+redeliver) has 3-day retention.
- `issue_comment` fires for the App's own comments — every consumer self-filters by sender.

**Current codebase (file:line in review transcripts):**
- **D12 is NOT complete today**: `/state/auto-advance` (`server.ts:2012-2066`) is a live second dispatcher gated by `LEGION_AUTO_ADVANCE`; the controller skill still branches on it. Removing it is Unit 0 of the execution plan — the single-writer regime below is false until it lands.
- Envoy `ENVOY_NOTIFICATIONS`: JetStream, `MaxAge` 1h default; `ensureStreamWithConfig` never updates an existing stream. Listener ACKs unmatched topics silently; dedupe cache records before delivery.
- Session delivery requires a live `envoy_sessions` registry entry (5m TTL); Envoy cold-start of serves was **removed deliberately for security** (`session.go:89`). The envoy plugin's 2-min heartbeat refreshes the TTL for ALL sessions ever busy in a serve, and `readoptSiblings` re-adopts after serve restart. The envoy plugin is present in the user's global OpenCode config; whether worker serves inherit it via config merge is verified empirically (§15) — the design makes it explicit regardless (§7.5).
- envoy-github **drops bot-authored** comment/review events before normalization — the exact events this design runs on. And it folds **all** `check_run` events into the debounced per-PR CI summary (`pr.<n>.ci` via cistore), returning before publishing raw events.
- The daemon already provides the dispatch-safety substrate: deterministic session IDs (`computeSessionId`, with a `version` field), 409 `worker_already_exists` on live-worker dispatch, and the `canDispatchMode` phase gate.
- `decision.ts` consumes `worker-done`, `worker-active`, `test-passed`, `test-failed`, `user-input-needed`, `needs-approval` labels as control flow.
- Auto-merge: `merge.auto_merge_allowed` is tracked in state, but no `gh pr merge --auto` call exists in code — App-token enablement is an empirical gate (§15).

## 3. Settled decisions

**The load-bearing regime premise:** ONE controller session (OpenCode processes wakes serially) + ONE daemon process (atomic worker-map ops) + all workers on ONE always-running shared serve + deterministic session IDs + 409-on-existing + phase gate. Given D12, there is no second concurrent decider — which is why the machinery below stays simple.

| # | Decision | Rationale |
|---|---|---|
| D1 | Go rewrite is still the destination | Unified codebase with Envoy, shared Go types, single binary (#394) |
| D2 | Strangler fig: new Go **collector** process (`packages/collector`) seeds the Go daemon; TS daemon loses organs by deletion, not porting | Migrations of moving targets go half-finished; rewrite settled things, build new things in the target language |
| D3 | Envoy stays a **generic** broker — no Legion domain logic. Envoy changes required by this design (§7) are generic broker features | Separation of concerns; Envoy stays reusable |
| D4 | Collector is a Legion component and an Envoy *client*, importing Envoy's Go types | Shared types without cohabitation |
| D5 | **One long-lived controller session**, woken by pushes | Cross-issue prioritization needs full context; the #693/#698 drift came from polling repetition, not longevity |
| D6 | No controller context-budget tracking, no forced rotation, **no watchdog, no wedge heuristics of any kind** | OpenCode compaction handles context. Turn-count/session-length "is it stuck" heuristics are banned; sessions are never auto-aborted or auto-re-seated on suspicion |
| D7 | GitHub remains source of truth. Collector's view is **read-only and disposable** | GitHub works fine today; avoids two-way sync entirely |
| D8 | Native issue store: **deferred**, explicit trigger (§13) | Revisit when structured-comment overloading / child tracking on GitHub measurably hurts |
| D9 | Children merge straight to main; **parent is coordination-only** (no PR, no branch). Hard rule. Glue work = explicit final child issue | Forces independently-shippable decomposition. Supersedes #707's open choice (§12) |
| D10 | Parent pipeline after last child closes: integrated acceptance **test** → holistic **review** of combined child diff → **architect reassessment** | The assembled feature was never validated; uniform phase sequence |
| D11 | Architect = the original session, resumed for the issue's whole life. **Memory is an optimization, never a correctness dependency** — gates are executable from durable issue artifacts alone | Original-session judgment is valuable; correctness can't hinge on transcript survival |
| D12 | **Controller is the sole dispatcher**, always via the daemon. `LEGION_AUTO_ADVANCE` and every daemon self-dispatch path are removed (**incomplete today — Unit 0**) | Single point of judgment; also the precondition that makes D22/D23's machinery unnecessary |
| D13 | Controller hears only **phase boundaries and exceptions** — never intra-issue traffic | cc-ing the controller on everything recreates noise-driven drift |
| D14 | Phase boundaries are **observable, durable GitHub artifacts**. Wakes are advisory; artifacts are truth. Every actor re-reads authoritative state on wake and decides from it | Observable artifacts beat unverifiable claims; gives emergent idempotency (a duplicate or lost wake causes at worst a wasted turn or a missing action healed by resync — never a wrong action) |
| D15 | **Subscriptions are the routing.** No central who-gets-this logic | Exactly-one-recipient by construction |
| D16 | Every worker gets a deterministic Envoy **role** `legion-<issue>-<mode>` at dispatch; workers get envoy-plugin tools; peer messaging by role, never session ID | Same mechanism as the existing `legion-controller` role |
| D17 | Subscription lifecycle is **daemon-owned and manifest-driven**: manifest persisted before worker start, reconciled until installed (subscribe failure = exception), torn down **only at issue terminal state**. Cross-mode unsubscribe on dispatch is removed | Subscriptions are the resumption mechanism |
| D18 | Reliability = read-then-decide + one artifact-driven resync pass with a guaranteed trigger (§8.4). No periodic mutate-on-diff reconciler, no DLQ subsystem, no delivery-accounting ledger | Each deleted mechanism guarded against a second concurrent decider that D12 eliminates; resync is the total correctness backstop |
| D19 | Draft PR = quiet; `ready_for_review` (or `opened` non-draft) = implement-phase boundary | Native GitHub semantics |
| D20 | Recursion via native sub-issue API; every issue gets its own architect; depth ≤ 8 enforced by GitHub. Acyclicity **verified empirically before build** (§15) | Tree guarantees mostly free; trust but verify |
| D21 | **Identity + gates**: implementer App authors PRs/commits/comments/sub-issues; reviewer App submits native reviews; tester and architect verdicts are check runs named **`tester`** and **`architect`** (bare names), each with **exactly one fixed publisher App**, required via branch protection. Never name a GitHub Actions job `tester` or `architect` | Native reviews need distinct identities; check runs have no author restriction and give mechanically enforced gates; source-pinning prevents spoofing |
| D22 | **Dispatch integrity = read-then-decide.** Before acting, re-read authoritative GitHub state. No CAS tokens, no per-issue phase epochs, no idempotency-key ledger — natural idempotency comes from checking current state (`check-run-exists-for-SHA`, `PR-already-merged`) before each write. Head-SHA fencing is native (new push resets checks to `expected`) | The multi-writer races this machinery guarded against don't exist in a single-decider regime (D12) |
| D23 | **Boundary handling**: the serial controller re-reads collector-materialized boundaries on wake; the collector's debounce (~10s) coalesces bursts; the LLM triages order itself. No durable claim/ack queue, no priority engine, no watchdog | One serial consumer already serializes; transcript ≠ work queue, but a queue subsystem is overbuild at this scale |
| D24 | **Architect veto routes to the controller**, not the implementer | A veto can mean rework, wrong plan, new child, or human escalation — classification is judgment |
| D25 | **Merge = GitHub native auto-merge** (`gh pr merge --auto --squash`) as the happy path; the merge worker shrinks to enabling auto-merge + handling blocked/base-changed cases. Retro dispatch stays post-merge (the controller/state machine observes the merged PR state), independent of merge mechanism | GitHub already merges the instant required checks + review are green; deleting bespoke merge logic |
| D26 | **Verdict signal carrier**: `tester`/`architect` check-run outcomes ride the existing debounced per-PR CI summary (`pr.<n>.ci`) — no special-cased raw publishing | Least new code, one coalesced signal shape; ~10s debounce is immaterial at Legion's tempo |

## 4. Process topology

**Today:** TS daemon (spawn workers, ports, workspaces, health) + Envoy (Go: webhook ingestion, NATS/JetStream, session delivery, roles).

**Transition:** + `packages/collector` (Go): subscribes to raw Envoy topics; materializes per-issue state in SQLite (append-only event log + current-state tables); coalesces; serves a read API that replaces `/state/collect` **behind a compatibility adapter** until dependents are ported. Child→parent relations are **not a separate stateful index** — derived on demand (GraphQL `parent`) and/or held inside the disposable materialized view. During the entire transition the **TS daemon remains the sole mutator of Envoy subscriptions**.

**End state:** one Go binary (Envoy subcommand + Legion daemon subcommand); TS daemon fully absorbed. Every intermediate state fully functional.

The collector's materialized view is the embryo of the future native issue store (D8): disposable until promoted.

## 5. Identity model (D21)

| Actor | GitHub identity | Writes |
|---|---|---|
| implementer, planner, architect-as-commenter, merge worker | implementer App | PRs, commits, issue/PR comments, sub-issue creation |
| reviewer | reviewer App | native PR reviews (approve / request changes) |
| tester | reviewer App (Sami 2026-07-19: reviewer App gets Checks:write and publishes both gates) | check run **`tester`** on head SHA (verdict + evidence link) |
| architect (gate) | reviewer App | check run **`architect`** on head SHA |

Branch protection requires: CI + `tester` + `architect` + one approving review — the check runs are pinned to their publisher App, but GitHub cannot restrict WHICH identity submits the approving review (source pinning exists for check runs only), so the workflow ensures the reviewer App is the approver. Check-run gates are enforced by GitHub; the approval-identity gate is enforced by workflow. Re-request review flows implementer-App → reviewer-App. All consumers of `issue_comment`/`check_run` self-filter events they authored (App slug / `app.id`).

## 6. Signal routing

### 6.1 Dispatch chain (controller = sole dispatcher; read-then-decide before every dispatch)

| Step | Boundary signal | Artifact (truth) | Carrier (wake) |
|---|---|---|---|
| 1 | Issue created / labeled for work → triage: architect track vs bug/planner track | the issue | GitHub event |
| 2 | Architect handoff: "leaf, ready to plan" or "decomposed into #A,#B" | structured issue comment (+ native sub-issues) | role msg to controller |
| 3 | Plan handoff → dispatch implementer | plan comment on issue | role msg to controller |
| 4 | PR ready → dispatch tester (enable auto-merge, D25) | PR non-draft | GitHub `ready_for_review`/`opened` |
| 5 | Tester PASS, no reviewer yet → dispatch reviewer | check run `tester` = success | CI summary (`pr.<n>.ci`, D26) |
| 6 | Review approved → *routed to architect, not controller* (gate) | native review by reviewer App | GitHub `pull_request_review` |
| 7 | Architect approval → all gates green → **GitHub auto-merges** (D25) | check run `architect` = success | CI summary (D26) |
| 8 | PR merged → dispatch retro (resume implement session) | merged PR | GitHub event |
| 9 | Parent: last child closed → architect reassess → "ready for integration test" → parent test → holistic review of combined child diff → gate → close | reassessment comment; parent verdicts as above | child `issues.closed` → architect; role msg to controller |

### 6.2 Reactive loops (no controller, no dispatch — existing sessions wake each other)

| Signal | Recipient | Action |
|---|---|---|
| Review: changes requested | implementer | fix, push, role-message tester for re-test |
| Re-test PASS (existing reviewer) | tester posts check run success, then re-requests review (as implementer App = PR author, allowed) | reviewer wakes via `review_requested` |
| Review re-requested | reviewer (same session) | re-review delta |
| Issue comment (human) | architect (owner) | answer / act / forward by role |
| PR comment / review comment | implementer | address |
| CI failed on PR | implementer | fix |
| CI passed on PR | tester (only if subscribed/waiting) | begin behavioral testing |
| Dispatch thread reply | the asking worker | continue |
| Child closed (not last) | parent's architect | note progress |
| Architect veto (`architect` check = failure) | **controller** (D24) | classify: implementer rework / planner rework / new child / human escalation |

### 6.3 Exception lane (deterministic detection → controller judgment; advisory only, never auto-abort)

| Condition | Detector |
|---|---|
| Worker crashed | daemon session/serve health (exists) |
| Worker went idle without producing its current-phase artifact | **objective conjunction**: (session emitted idle/turn-complete transition) AND (expected-phase artifact absent). Advisory to controller only. No turn counts, no timeouts, never auto-abort/re-seat. When the expected artifact is ambiguous, do not advise |
| Role message with no holder, or failed delivery | Envoy listener raises an exception instead of silently ACKing (§7.1); dedupe keys on successful delivery |
| Issue non-terminal with no live owner (incl. dormant parent architects, human-blocked > N days) | the resync pass (§8.4) — one pass, not separate detectors |
| Repeated crashes on the same worker | daemon crash counter (objective, deterministic) |

### 6.4 Subscription manifests (installed by daemon at dispatch, per D17)

| Role | Subscribed to |
|---|---|
| architect | issue comments; issue events; `sub_issues` events; child `issues.closed`; `pull_request_review` (approved) on the issue's PR |
| planner | (architect forwards; direct subscription only if empirically needed) |
| implementer | PR comments, review comments, changes-requested reviews, CI-fail on its PR; dispatch replies |
| tester | CI summary on the PR; re-test role messages |
| reviewer | `review_requested` on the PR |
| controller | `legion-controller` role; issue-created; exception lane; collector digests |

All consumers filter self-authored events. Workers re-read authoritative state on wake and no-op if the action is already done (D14/D22).

## 7. Delivery semantics — required Envoy work (generic features, per D3)

1. **No silent drops on control topics.** A message with no holder, or a failed delivery, raises a consumable exception (routed to the controller's exception lane) instead of a silent ACK. Dedupe keys on **successful** delivery, not attempt. Deliberately small — the resync pass (§8.4) is the correctness backstop.
2. **Retention.** Raise control-flow `MaxAge` (value chosen against the inspected cluster, §15); make `ensureStreamWithConfig` update existing streams on config drift; migrate the deployed stream explicitly.
3. **Bot-event filtering becomes downstream.** Remove the blanket drop of bot-authored comment/review events (blocking — the routing matrix runs on them); consumers self-filter by sender.
4. **Worker-serve heartbeat wiring (the real "§7.5").** Add `@sjawhar/opencode-legion-envoy` to the daemon's injected must-load plugin list so worker delivery never depends silently on the user's global config. The plugin's existing 2-min heartbeat keeps every session's `envoy_sessions` TTL fresh for the life of the serve; `readoptSiblings` re-adopts after restart. **Do NOT rebuild an Envoy cold-start/resume path — it was removed deliberately for security.** Empirical gate: an idle worker stays deliverable past the 5-min TTL (§15).

## 8. Dispatch and state integrity

1. **Wakes advisory, artifacts truth (D14/D22).** Actors re-read authoritative GitHub state on wake; no CAS, no epochs.
2. **Natural idempotency.** Check current GitHub state before each side-effectful write (`check-run-exists-for-SHA`, `PR-already-merged`). No idempotency-key ledger.
3. **Serial controller.** Re-reads collector-materialized boundaries on wake; collector debounce coalesces; no queue subsystem (D23).
4. **Artifact-driven resync/repair — the workhorse.** A **read-only** pass re-derives the expected next-owner for every non-terminal issue from GitHub artifacts alone, cross-references live workers, and emits "artifact exists but no live owner → re-dispatch" (executed via the controller). Subsumes zero-holder repair, ownership audit, missed-webhook recovery, and the human-blocked report. **Guaranteed trigger (I4): invoked on daemon restart and by a low-frequency timer** — a periodic invocation of the read-only repair pass, explicitly NOT the rejected continuous mutate-on-diff reconciler, and NOT wedge-detection.

## 9. Recursion + architect ownership

- #705 as designed: native sub-issue API; every issue gets its own architect; the worker-done-skips-architect special case is removed. Phase 0 prerequisite: GitHub App `Issues: write` + installation re-approval.
- Architect duties: decompose or declare leaf; record decomposition rationale + **parent-level acceptance criteria** at decomposition time; own issue comments; reassess on child closures; final-approval check run before merge; parent reassessment after the integrated test+review.
- Architect session is resumed for all of this (D11), but every gate is executable from durable artifacts alone.
- Session retention classes: owners of open issues stay resumable indefinitely; terminal-issue sessions are archived/unsubscribed at close.

## 10. Controller contract

Wakes on: boundary digests, exception lane, issue-created (triage), human directives. On wake: re-read authoritative state → decide → dispatch via daemon → sleep. The controller skill sheds all polling instructions and intra-issue relaying; it becomes triage rules + dispatch procedures + exception playbook + prioritization judgment.

## 11. Migration (tear-out with replacements — nothing removed before its replacement lands)

| Today | Replacement | Order |
|---|---|---|
| `LEGION_AUTO_ADVANCE` + `/state/auto-advance` (live second dispatcher) | removed outright (D12) | **Unit 0 — first, precondition for resync work** |
| `worker-done` label (decision input) | boundary artifacts per §6.1 | after §6 signals live |
| `worker-active` label | daemon worker registry via collector read API | after collector read API |
| `test-passed` / `test-failed` labels | `tester` check run | with D21 |
| `user-input-needed` / `user-feedback-given` labels | dispatch threads + resync human-blocked report | after resync |
| `needs-approval` / `human-approved` labels | `architect` check run + branch protection (obsoletes #694) | with D21 |
| controller `/state/fetch-and-collect` polling | boundary wakes + collector read API; compat adapter until dependents ported | staged |
| cross-mode unsubscribe on dispatch; unsubscribe on dead/deleted/Done-cleanup | manifest-driven teardown at terminal state only (D17) | with subscription-lifecycle work |
| worker self-unsubscribe on `session.deleted` (envoy-plugin) | daemon-owned teardown | with subscription-lifecycle work |
| bespoke merge execution in merge worker | native auto-merge (D25); worker shrinks to enable + handle blocked | after branch protection (D21) |

## 12. Supersessions (explicit)

- **#394**: "issue tracker built into daemon" and "bidirectional GitHub sync" **deferred**, not abandoned (D7/D8). Go rewrite restarts as the collector strangler. Everything else in #394 §7 stands.
- **#707**: parent-integration semantics settled by D9/D10. Architect gate kept and strengthened (check-run enforcement).
- **#706**: controller issue-event subscription absorbed into collector design.
- **docs/plans/2026-04-06-envoy-subscription-lifecycle.md**: superseded where it conflicts with D17 (self-unsubscribe on exit); its gap inventory remains valid input.
- **Spec v2 (this document's prior revision)**: CAS/phase-epoch dispatch protocol, DLQ + delivery-accounting ledger, idempotency-key ledger, durable priority claim/ack controller queue, controller watchdog/re-seat, and "rebuild Envoy cold resume" are all **withdrawn** — they solved multi-writer races that D12's single-decider regime eliminates, or (cold resume) misdiagnosed a heartbeat wiring gap.

## 13. Deferred, with explicit triggers

| Item | Trigger |
|---|---|
| Native issue store (promote collector view; port dispatch backend) | structured-comment overloading or GitHub child tracking becomes the measured bottleneck |
| Decomposition-pattern learning (#708) | after recursion produces real decomposition data |
| GitHub deliveries-API redelivery checker | measured webhook-loss latency actually hurts (resync already heals correctness) |
| Author/bot-based comment routing config | empirical misrouting of architect-default issue-comment routing |
| Multi-machine architecture (#394 §6) | second machine joins under the new architecture |

## 14. Execution (routing/delivery layer — planned via hyperplan 2026-07-19)

Units, dependency-ordered; one PR each; TDD; details in the hyperplan output:

- **U0** remove auto-advance (D12 — precondition for U7/U8/U9) · **U1** this spec revision · **U2** Envoy: stop dropping bot-authored events (blocking, ships first) · **U3** JetStream retention migration (gated: inspect deployed config) · **U4** heartbeat: add `opencode-legion-envoy` to daemon's injected plugins (gated: idle-worker deliverability test) · **U5** Checks:write grant + pinned bare checks + branch protection (human-gated, kick off day 0) · **U6** verdict carrier via CI summary (D26; gated: check_run round-trip test; needs U2+U5) · **U7** artifact-driven resync pass (needs U0) · **U8** guaranteed resync trigger (needs U7) · **U9** idle-worker advisory (needs U7; the subtle unit — per-mode expected-artifact map encoded as data; advise-never-abort) · **U10** native auto-merge adoption (needs U5; gated: App-token enablement test) · **U11** no-holder/failed-delivery exception.

Parallel tracks: ENVOY U2→{U3,U11}→U6 · DAEMON U0→U7→{U8,U9}, U4 anytime · GITHUB U5→U10 · critical path = U5 (human re-approval latency) and U0→U7→U9.

Remaining workstreams (separate plans): collector v1, subscription-lifecycle manifests, controller skill rewrite, recursion semantics (#705/#707), label tear-out + backlog restructure.

## 15. Open items requiring empirical verification (gate their dependent units, not the spec)

1. Sub-issue cycle rejection behavior (scratch-repo test) — gates recursion plan
2. Deployed JetStream stream config vs code defaults — gates U3
3. Legion `check_run` round-trip through envoy-github / CI-summary inclusion of `tester`/`architect` — gates U6
4. Auto-merge enablement under a GitHub App installation token — gates U10
5. Idle worker (>5 min) remains deliverable once the envoy plugin is in the worker serve's plugin list — gates U4
