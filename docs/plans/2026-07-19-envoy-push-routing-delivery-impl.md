# Envoy Push — Routing/Delivery Layer Implementation Plan

> Status: ready for review
> Date: 2026-07-19
> Design record: `docs/plans/2026-07-18-envoy-push-architecture-design.md` (v3, §14 summarizes; unit detail from the hyperplan round)
> Scope: implements units U0–U11 of the routing/delivery layer. End state = **ONE PR** containing all units plus the spec v3 doc commits already on this jj stack.
> Verification is line-for-line anchored against the current tree (audited 2026-07-19). Anchor corrections vs the hyperplan unit file are in the **Anchor drift** section.

This plan is executed by fresh implementer subagents with zero prior context. Every task states exact files, a failing test to write first, implementation steps, and verification commands. Each task **ends at "verification green"** — implementer subagents NEVER run version-control commands. The coordinator commits (see **Commit protocol**). Follow tasks in the **Dispatch order** waves.

---

## Global constraints (binding — apply to every task)

**Banned machinery — do NOT introduce any of these anywhere in this PR:**
- No CAS tokens on the daemon dispatch path (the pre-existing CAS inside `internal/cistore` KV aggregation is not part of this — do not add new CAS).
- No phase epochs.
- No DLQ / delivery-accounting ledger.
- No idempotency-key ledger.
- No priority claim/ack queue.
- No controller watchdog.
- No Envoy cold-start / cold-resume rebuild (removed deliberately for security — `internal/session/session.go:89`).
- No turn-count / session-length "is it stuck" wedge heuristics anywhere.
- **NEVER auto-abort or auto-re-seat a session.** All new detectors are **advisory only** — they emit a recommendation to the controller; the controller (a human-judgment LLM) decides. This is the **advisory-never-abort** rule.

**Naming / identity:**
- Bare check-run names are exactly `tester` and `architect`, published by the reviewer App. **Never name a GitHub Actions job `tester` or `architect`** (collision produces ambiguous required checks).
- Verdicts (`tester`/`architect` check-run outcomes) ride the existing debounced per-PR CI summary (`pr.<n>.ci` via `internal/cistore`, D26). **No special-cased raw `check_run` publishing.**

**Branch protection:**
- Branch protection requiring CI + `tester` + `architect` + reviewer-App approval is **PREPARED + DOCUMENTED but NOT APPLIED to `main`** in this PR. Applying it now would require those checks to exist and would block this very PR from merging. Ship the `gh api` command / script committed to the repo; apply manually after the pipeline can post the checks.

**Repo hygiene:**
- **Do NOT touch `.opencode/package-lock.json`** (pre-existing working-copy drift, not ours).
- **jj, never git.** All committing is done by the **coordinator** per the **Commit protocol** section — never by implementer subagents, and never `git` (`git add`/`git commit`/`git push`/`git rebase` are all forbidden).
- One PR at the end; the coordinator squashes the wave commits into that single PR.

**Verification command matrix:**
- TypeScript (`packages/daemon`): `bun test` · `bunx tsc --noEmit` · `bunx biome check src/`
- Go (`packages/envoy`): `go test ./...` (or the scoped `go test ./internal/<pkg>/...` named per task), run **from `packages/envoy`**.

**Empirical (emp) probes:** several tasks are gated on live-environment probes. Each probe is written as an explicit step. **If no live environment is reachable from CI, record the exact deploy-time verification command in the task's commit message and in the PR description** — do not block the code change on the probe.

---

## Commit protocol (coordinator-only)

**Implementer subagents NEVER run any version-control command.** They stop at "verification green" and report. This is because parallel `jj describe`/`jj new` in one shared working copy race and corrupt the change graph.

The **coordinator** serializes all commits:
1. Dispatch a wave's parallel groups. Each implementer reports back with verification output (tests/tsc/biome or `go test`) green.
2. When **all** groups in the wave report green, the coordinator runs exactly one commit for the wave in the shared working copy:
   `jj describe -m "<wave summary listing the units>"` then `jj new`.
3. Only then dispatch the next wave.
4. After Wave 3, the coordinator opens the single PR (the wave commits + the already-present spec v3 commits on this stack).

Suggested wave commit messages:
- Wave 1: `feat: remove auto-advance + heartbeat plugin (U0,U4); stop bot-event drop (U2); control-topic exceptions (U11); JetStream retention (U3); prepare required checks (U5)`
- Wave 2: `feat: read-only resync recommendations (U7); verdict carrier via CI summary (U6); native auto-merge (U10)`
- Wave 3: `feat: guaranteed resync trigger (U8); idle-worker advisory (U9)`

Never run `git` for any of this. Never let a subagent commit.

---

## Dispatch order (waves; disjoint file sets so the coordinator can verify no overlap)

Groups within a wave run in parallel. Each group's file set is listed; sets are disjoint within a wave.

### Wave 1 (parallel)
| Group | Task(s) | Files touched (disjoint) |
|---|---|---|
| **A** | Task 1 — U0 + U4 | `packages/daemon/src/daemon/server.ts`, `packages/daemon/src/daemon/config.ts`, `packages/daemon/src/daemon/index.ts`, `packages/daemon/src/daemon/__tests__/*` (new + advance.test.ts), `.opencode/skills/legion-controller/SKILL.md` |
| **B** | Task 2 — U2 · Task 3 — U11 (may be one dispatch) | `packages/envoy/internal/webhook/github.go` (+ `github_test.go`); `packages/envoy/cmd/listener/main.go` (+ `main_test.go`); `packages/envoy/internal/contracts/generated.go` + `packages/contracts/*` generator source (U11 exception envelope — see Task 3) |
| **D** | Task 4 — emp#2 + U3 | `packages/envoy/internal/bus/nats.go` (+ `internal/bus/*_test.go`) |
| **E** | Task 5 — U5 remainder | `scripts/` (new gh-api probe + branch-protection script), `docs/` (branch-protection runbook). No `src/` overlap. |

Wave 1 disjointness: A is daemon TS + controller skill; B is envoy `webhook`+`listener`+`contracts`; D is envoy `bus`; E is scripts/docs. B and D are both envoy Go but in different packages (`internal/webhook`+`cmd/listener`+`internal/contracts` vs `internal/bus`) — no shared file. (U2 and U11 are one group because both are envoy Go; U11 additionally touches `internal/contracts`, which U2 does not.)

### Wave 2 (parallel, after Wave 1)
| Group | Task | Files touched (disjoint) | Depends on |
|---|---|---|---|
| **F** | Task 7 — U7 | `packages/daemon/src/daemon/resync.ts` (new), `packages/daemon/src/daemon/phase-artifacts.ts` (new — PhaseArtifacts fetch contract), `packages/daemon/src/daemon/__tests__/resync.test.ts` (new) | U0 (Task 1) landed |
| **G** | Task 6 — U6 | `packages/envoy/internal/cistore/cistore.go` (+ `*_test.go`), `packages/envoy/internal/contracts/normalize.go` (+ `normalize_test.go`), `packages/envoy/internal/webhook/github.go` (+ `github_test.go`), `internal/cistore/render.go` if payload gap found | U2 (Task 2) landed |
| **H** | Task 8 — U10 | `.opencode/skills/legion-worker/workflows/merge.md`, `docs/` merge runbook | U5 probe (Task 5) |

Wave 2 disjointness: F is daemon TS (new file); G is envoy cistore/webhook tests; H is a skill md + docs. No shared file.

### Wave 3 (parallel, after F)
| Group | Task | Files touched (disjoint) | Depends on |
|---|---|---|---|
| **I** | Task 9 — U8 | `packages/daemon/src/daemon/index.ts`, `packages/daemon/src/daemon/config.ts`, `packages/daemon/src/daemon/__tests__/index.test.ts` (extended) | U7 (Task 7) landed |
| **J** | Task 10 — U9 | `packages/daemon/src/daemon/idle-advisory.ts` (new) + test, `packages/daemon/src/daemon/resync.ts` (extend to invoke it). Reuses `phase-artifacts.ts` from U7 (read-only import; not edited). | U7 (Task 7) landed |

Wave 3 disjointness (the one to watch): **U8 edits `index.ts` + `config.ts` ONLY** (schedules the periodic + startup trigger that calls `runResyncPass`). **U9 edits `resync.ts` + new `idle-advisory.ts` ONLY** (adds idle-advisory computation *inside* `runResyncPass`, plus the pure module). U8 never imports the idle-advisory symbol directly — it only schedules `runResyncPass`, which U9 extends. Because `resync.ts` was created by U7 in Wave 2, U9 is the sole Wave-3 editor of it. `index.ts` (U8) and `resync.ts`/`idle-advisory.ts` (U9) are distinct files → no conflict.

---

## Anchor drift found (corrections vs `/tmp/opencode/hyperplan-units.md`)

Confirmed exact (no drift): `server.ts:2012-2066`, `server.ts:90`, `index.ts:203-205`, `index.ts:587`, `config.ts:41`, `config.ts:593-596`, `config.ts:658`, `config.ts:744-749`, `nats.go:146-156`, `github.go:100-114` (CI fold), listener `main.go:831-835` / `793-805` / `845-855`.

Drift / additions the implementer must apply:
1. **U0 `config.ts` — two extra removal sites not cited by the unit.** Besides 41 / 593-596 / 658 / 744-749, `auto_advance` also appears at **`config.ts:116`** (`auto_advance: null,` inside the RECOGNIZED-keys defaults object, lines ~104-120) and at **`config.ts:840`** (`autoAdvance: autoAdvance.value,` inside the resolved-config output object, lines ~835-842). Both must be deleted or `grep -ri 'auto.?advance'` will not come clean and the resolved object will reference a removed binding.
2. **U0 `SKILL.md` — strip the whole subsection.** The unit cited `853-867`; the `#### Auto-Progression` heading is at **`851`** (blank line `852`). Strip **851–867** so the heading + fenced bash block go together.
3. **U2 `github.go` — precise early-return is `88-93`, not `83-93`.** The bot-sender drop is `if contracts.GithubIsBotSender(payload) { ... return }` at **lines 88-93**, nested inside `if githubEvent(event) { <log 84-87> ... }` (the `githubEvent` block spans 83-94). **Delete only 88-93**; keep the sender log (84-87) and the `githubEvent` wrapper. Additionally: **`githubEvent()` (github.go:17-23) returns true ONLY for `issue_comment`, `pull_request_review_comment`, `pull_request_review` — NOT `check_run`.** Therefore the bot-drop never touched `check_run` events; app-created `tester`/`architect` check runs already reach the CI fold. This **softens** "U6 needs U2": U2 is still required for the routing-matrix comment/review events, but it was never the thing blocking check-run recording. Verify with the emp#3 round-trip test in Task 6.
4. **U6 needs publisher identity (per coordinator ruling on finding 3), so it is NOT purely test-only.** `internal/cistore/render.go` `RenderSummary` emits `Summary{Passed/Failed/Running/Queued/Skipped: StatusGroup{Count, Checks []string}}`; each check appears by **name** in its group, so a `tester=success` lands in `Summary.Passed.Checks=["tester"]`. But `internal/contracts/normalize.go` `CIObservation` (75-83) carries no `app.id`, and `internal/cistore/cistore.go` `Check` (44-49) stores only `Status`/`Conclusion`/`UpdatedAt`. D21 requires each verdict to come from a **fixed publisher App**. So Task 6 must thread the check-run publisher `app.id` into the CI-recording path and record `tester`/`architect` observations **only** when published by the pinned reviewer App — otherwise a wrong-App `tester` check run must NOT satisfy the verdict. Exact plumbing is in Task 6.

---

## Task 1 — U0 (remove auto-advance) + U4 (heartbeat plugin) [Wave 1, group A]

**Why:** U0 makes the controller the sole dispatcher (precondition for U7/U8/U9). U4 makes worker delivery independent of the user's global OpenCode config. Both edit `index.ts`, so they are one task to avoid a self-conflict.

**Files:**
- `packages/daemon/src/daemon/server.ts` (remove route + option)
- `packages/daemon/src/daemon/config.ts` (remove config field + parse + resolve + output)
- `packages/daemon/src/daemon/index.ts` (remove pass-through @587; add envoy plugin @204)
- `.opencode/skills/legion-controller/SKILL.md` (strip Auto-Progression subsection)
- `packages/daemon/src/daemon/__tests__/advance.test.ts` (extend), and its `startTestServer` no longer passes `autoAdvance`

**TDD (write failing tests first):**
- [ ] In a new/extended daemon server test (reuse the `advance.test.ts` harness: `startServer(...)` with the DI adapter that records `createSessionCalls`), add: `it("POST /state/auto-advance returns 404")` — assert `response.status === 404`.
- [ ] Add: `it("does not dispatch without explicit /workers or /state/advance")` — seed a `Todo` issue via `POST /state/collect` (see `seedIssueInCache`), then assert `createSessionCalls.length === 0` after collection with no advance call.
- [ ] Add a `buildServeEnv` unit assertion (extend `index.test.ts` or a small focused test) that the injected `OPENCODE_CONFIG_CONTENT` plugin array contains **both** `@sjawhar/opencode-legion@latest` **and** `@sjawhar/opencode-legion-envoy@latest`.
- [ ] Run the tests; confirm they fail (route still 200; env has one plugin).

**Implementation — U0:**
- [ ] `server.ts`: delete the entire `POST /state/auto-advance` handler and its inline `AUTO_ADVANCE_ACTIONS` set — lines **2012-2066** (from the `// POST /state/auto-advance` comment through its closing `}`).
- [ ] `server.ts`: delete the `autoAdvance?: boolean;` option + its doc comment on `ServerOptions` — lines **89-90** (`/** Auto-advance... */` + the field).
- [ ] `config.ts`: delete `autoAdvance: boolean;` from `DaemonConfig` — line **41**.
- [ ] `config.ts`: delete `auto_advance: null,` from the RECOGNIZED defaults object — line **116** (drift #1).
- [ ] `config.ts`: delete the YAML parse block — lines **593-596** (`const autoAdvance = readBoolean(parsed.auto_advance, "auto_advance");` + the `if` that sets `fields.autoAdvance`).
- [ ] `config.ts`: delete the env parse — line **658** (`const envAutoAdvance = parseOptionalBoolean(env.LEGION_AUTO_ADVANCE);`).
- [ ] `config.ts`: delete the resolver block — lines **744-749** (`const autoAdvance = resolveValue(... false);`).
- [ ] `config.ts`: delete `autoAdvance: autoAdvance.value,` from the resolved-config output object — line **840** (drift #1).
- [ ] `index.ts`: delete the pass-through `autoAdvance: config.autoAdvance,` in the `startServer({...})` call — line **587**.
- [ ] `advance.test.ts`: remove `autoAdvance` from the `startTestServer` options type and the `startServer` call (lines ~77, ~95) so the harness compiles after the option is gone.
- [ ] `.opencode/skills/legion-controller/SKILL.md`: strip the `#### Auto-Progression` subsection — lines **851-867** (drift #2).
- [ ] **KEEP untouched:** `POST /state/advance`, `canDispatchMode`, the `409 worker_already_exists` path, and `computeSessionId`.

**Implementation — U4:**
- [ ] `index.ts`: in `buildServeEnv`, change the plugin array (line **204**) from `plugin: ["@sjawhar/opencode-legion@latest"],` to `plugin: ["@sjawhar/opencode-legion@latest", "@sjawhar/opencode-legion-envoy@latest"],`.

**emp#5 probe (U4 deliverability — deploy-time if no live daemon):**
- [ ] If a live daemon is reachable: dispatch a worker; let it idle > 5 min; confirm the `envoy_sessions` KV still holds its entry and a role message is delivered.
- [ ] If not reachable in CI: record this exact deploy-time verification in the commit message + PR description: *"After deploy: dispatch a worker, idle > 5 min, confirm `envoy_sessions` KV entry persists and a `legion-<issue>-<mode>` role message is delivered."*

**Verification:**
- [ ] Run **exactly** `grep -RniE 'auto[-_]?advance' packages/ .opencode/` and confirm the result is **empty of live code and SKILL.md control flow** (only `docs/` historical references may remain). `-i` makes it case-insensitive and `[-_]?` makes the separator optional, so this single pattern matches `auto_advance`, `auto-advance`, `autoAdvance`, `autoadvance`, and `AUTO_ADVANCE` — including the camelCase sites at `config.ts:41`, `config.ts:840`, and `index.ts:587`. (A plain `grep '?'` is literal and would miss these.)
- [ ] `bun test` green (advance.test.ts + new tests pass).
- [ ] `bunx tsc --noEmit` clean.
- [ ] `bunx biome check src/` clean.

**Task ends at verification green.** Do NOT commit — the coordinator commits the wave (see Commit protocol).


## Task 2 — U2 (Envoy: stop dropping bot-authored events) [Wave 1, group B] — BLOCKING, highest priority

**Why:** the routing matrix runs on `issue_comment` / `pull_request_review` / `pull_request_review_comment` events authored by the implementer/reviewer Apps (bots). Today they are dropped before normalization. Consumers self-filter downstream (no consumer change in this unit).

**Files:** `packages/envoy/internal/webhook/github.go` (+ `github_test.go`).

**TDD (write failing test first):**
- [ ] In `internal/webhook/github_test.go`, add a table case: feed a Bot-sender `issue_comment` fixture (sender `type: "Bot"`) through `GitHubHandler`; assert the publisher **receives an envelope** (today it asserts the event is dropped / no publish). Mirror the existing handler-test setup (mock `Publisher`, mock `CIRecorder`).
- [ ] Run `go test ./internal/webhook/...`; confirm it fails (event dropped, no envelope).

**Implementation:**
- [ ] `github.go`: delete the bot-sender early-return — lines **88-93** (drift #3): the `if contracts.GithubIsBotSender(payload) { log...; w.WriteHeader(200); w.Write("ok"); return }` block. **Keep** the sender log at 84-87 and the `if githubEvent(event) {...}` wrapper (83, 94). After the edit the block reads: `if githubEvent(event) { log.Printf("github sender ...") }`.
- [ ] Do not change `githubSkip`, the CI fold (100-114), or the envelope path.

**Verification:**
- [ ] `go test ./internal/webhook/...` green (run from `packages/envoy`).
- [ ] `go test ./...` green from `packages/envoy`.

**Task ends at verification green.** Do NOT commit — the coordinator commits the wave (see Commit protocol).
---

## Task 3 — U11 (Envoy: no-holder / failed-delivery → exception) [Wave 1, group B]

> May be dispatched together with Task 2 (same group B), but is a distinct file (`cmd/listener/main.go`) so it can also be a separate worker.

**Why:** today a control-topic message with no holder is silently ACKed (both the fanout `registry.Match` path AND the direct `notifications.agent.<session>` path), and a failed delivery is suppressed on retry by the pre-delivery `attemptCache`. The routing design needs a consumable exception instead of a silent drop, and failed deliveries must survive to retry — **without losing the in-flight duplicate guard** that prevents slow-204 phantom duplicates (`MaxAckPending(256)` at main.go:63; rationale at main.go:739).

**Files:** `packages/envoy/cmd/listener/main.go` (+ `cmd/listener/main_test.go`).

**Definitions:**
- **Control topic** = `notifications.role.*` (role subjects; matched via `registry.Match` fanout) **OR** `notifications.agent.*` (direct session subjects; `AgentTopicPrefix = "notifications.agent."`, generated.go:57). Add an exported helper `isControlTopic(topic string) bool` (in the listener or a small contracts helper) recognizing both prefixes. There is no exported role-prefix constant today (`"notifications.role."` is a raw string at `internal/store/kv.go:220`); add `RoleTopicPrefix = "notifications.role."` to `internal/contracts` alongside `AgentTopicPrefix` and use it here and in kv.go.

**Relevant current anchors (confirmed):**
- Direct-agent no-holder (agent path): main.go:**819-822** — when `result.Delivered == false && result.Err == nil` the message is logged "session not found anywhere" and ACKed. This is a no-holder for `notifications.agent.<session>` and must raise an exception.
- Fanout no-holder: main.go:**831-835** (`items := registry.Match(...)` → `if len(items) == 0 { log; msg.Ack(); return }`).
- Agent-path attempt dedupe: `attemptCache.Seen` at **793**, `attemptCache.Record` at **804** (before `session.HandleAgentMessage` at 806); permanent `dedupeCache.Record` after success at **817**.
- Fanout-path attempt dedupe: `attemptCache.Seen` at **845**, `attemptCache.Record` at **855** (before `deliver.Deliver` at 857); permanent `dedupeCache.Record` after success at **869**.
- `attemptCache := dedupe.New(5*time.Minute)` at **746**; `dedupeCache := dedupe.New(10*time.Minute)` at **737**. `dedupe` has `Seen`/`Record`; add a `Clear(key, session)` (or `Forget`) method if none exists so a failed attempt can be un-marked (check `internal/dedupe`).
- The handler closure has the bus client (`client`, with `client.JS()`); publish the exception through it. Reuse the existing envelope encoder — do not invent a new one.

**TDD (write failing tests first):**
- [ ] `main_test.go`: **fanout no-holder** — publish on a `notifications.role.<x>` control topic with zero matching subscribers; assert an **exception event** is emitted on `notifications.envoy.exceptions.notifications.role.<x>` (subscribe a probe consumer and read it). Today: silent ACK, nothing emitted.
- [ ] `main_test.go`: **direct-agent no-holder** — publish on `notifications.agent.<session>` where the session is not live (`result.Delivered==false, Err==nil`); assert an **exception event** is emitted on `notifications.envoy.exceptions.notifications.agent.<session>`. Today: main.go:819-822 ACKs silently.
- [ ] `main_test.go`: **in-flight duplicate still suppressed** — while a delivery is in flight (before success), a second envelope with the same `(DedupeKey, session)` is skipped (the slow-204 guard — must survive this change).
- [ ] `main_test.go`: **failed delivery is retried** — a delivery that fails and is NAK'd is **not** suppressed on redelivery (the in-flight marker was cleared on failure).
- [ ] `main_test.go`: **successful delivery still deduped** — a delivered message records permanent dedupe (a later redelivery of the same DedupeKey is skipped).
- [ ] Run `go test ./cmd/listener/... ./internal/contracts/...`; confirm failures.

**Implementation:**
- [ ] **Exception envelope (exact shape).** Extend `internal/contracts` `Envelope.Validate` (generated.go:49-53) to allow `Source == "envoy"` in the whitelist. **Contracts are generated** — find the generator source (check `packages/contracts/*` for the TS-side schema that emits `internal/contracts/generated.go`); add `"envoy"` there and regenerate, rather than hand-editing generated output. If no active generator maps to this file, edit `generated.go` directly and note it. Emit the exception as an `Envelope` with: `Source="envoy"`; `Topic="notifications.envoy.exceptions."+<original-topic>` (**finding 1: this prefix keeps the exception INSIDE the `ENVOY_NOTIFICATIONS` stream, whose subjects are `notifications.>` at nats.go:19 — an `envoy.exceptions.*` subject falls OUTSIDE the stream and would never persist. Do NOT widen the stream subjects; that would tangle with the U3 migration.**); `Payload` (or `PayloadSummary`) carrying `{ original_topic, event_id (the original EventID), reason: "no_holder"|"delivery_failed", payload_summary (original PayloadSummary) }`; fresh `EventID`/`DedupeKey`/`IssuedAt`/`TraceID`. Keep it generic — no Legion domain fields. The controller subscribes to `notifications.envoy.exceptions.>`.
- [ ] **Fanout no-holder** (832-835): when `len(items) == 0` **and** `isControlTopic(item.Topic)`, publish the exception (reason `no_holder`) via `client` before ACKing. Non-control topics keep the existing quiet ACK.
- [ ] **Direct-agent no-holder** (819-822): in the `result.Delivered==false && result.Err==nil` branch, when `isControlTopic(item.Topic)` (agent subjects are control topics), publish the exception (reason `no_holder`) before ACKing.
- [ ] **In-flight guard + failed-delivery retry (finding 1, keep semantics exactly).** Keep the pre-delivery `attemptCache.Record` (804 / 855) as the **in-flight marker** so slow-204 phantom duplicates are still suppressed. On a **retryable failure / NAK** (the `result.ShouldNAK` / `failed` paths at 823-828 agent and 861-864/875-880 fanout), **clear** the in-flight marker for that `(DedupeKey, session)` via a **mutex-protected** `dedupe.Clear`, called **before** `NakWithDelay` (oracle-confirmed race-free under this ordering), so the redelivery is processed. Record **permanent** dedupe (`dedupeCache.Record`) **only on success** — which is already the case at 817 / 869; do not move it. On a failed delivery also publish an exception (reason `delivery_failed`).
- [ ] Preserve the existing `ShouldNAK` / `NakWithDelay(30s)` behavior for retryable failures.

**Verification:**
- [ ] `go test ./cmd/listener/... ./internal/...` green (from `packages/envoy`), including the in-flight-suppression, failed-retry, and both no-holder tests.

**Task ends at verification green.** Do NOT commit — the coordinator commits the wave (see Commit protocol).

---

## Task 4 — emp#2 probe + U3 (Envoy: JetStream retention migration) [Wave 1, group D]

**Why:** control-flow messages must survive long enough for the resync backstop; today `ENVOY_NOTIFICATIONS` `MaxAge` defaults to ~1h and `ensureStreamWithConfig` never updates an existing stream.

**Files:** `packages/envoy/internal/bus/nats.go` (+ `internal/bus/*_test.go`).

**emp#2 probe FIRST (choose the new MaxAge against reality):**
- [ ] If the deployed NATS cluster is reachable: inspect the live `ENVOY_NOTIFICATIONS` stream config (`nats stream info ENVOY_NOTIFICATIONS`, or the `packages/envoy/scripts/verify-cluster.sh` path) to learn the actual deployed `MaxAge`. Choose the new value against it.
- [ ] If not reachable: choose **72h** (suggested default; nothing observed contradicts it) and record in the commit message: *"Deploy-time: run `nats stream info ENVOY_NOTIFICATIONS`, confirm MaxAge before/after listener restart."*

**TDD (write failing test first):**
- [ ] `internal/bus` test: pre-create the stream with an *old* config (short MaxAge), then call `ensureStreamWithConfig` with the new `streamCfg`; assert the stream config is **updated** to the new MaxAge. Today `ensureStreamWithConfig` returns early when the stream exists (nats.go:148-150), so this fails.
- [ ] Run `go test ./internal/bus/...`; confirm failure.

**Implementation:**
- [ ] Raise the control-flow `MaxAge` on `streamCfg` (the `Stream` config) to the chosen value (72h unless the probe says otherwise).
- [ ] `nats.go` `ensureStreamWithConfig` (146-156): when `StreamInfo` succeeds (stream exists), compare the live config against `cfg`; if it drifts (e.g. MaxAge differs), call `js.UpdateStream(cfg)` and return its error. Keep the `AddStream` path for `ErrStreamNotFound`. Document (comment) that the deployed stream migrates automatically via this `UpdateStream` on the next listener start.

**Verification:**
- [ ] `go test ./internal/bus/...` green (from `packages/envoy`).
- [ ] If the live cluster is reachable: after a listener restart, `nats stream info ENVOY_NOTIFICATIONS` shows the new MaxAge (else recorded as deploy-time verification per the probe step).

**Task ends at verification green.** Do NOT commit — the coordinator commits the wave (see Commit protocol).

---

## Task 5 — U5 remainder (pinned bare required checks + branch-protection prep) [Wave 1, group E]

**Why:** Sami already granted `Checks: write` to the reviewer App (installation re-approval accepted). Remaining: verify the App can create a `tester` check run, and **prepare + document** branch protection **without applying it**.

**Files:** `scripts/` (new gh-api probe + a branch-protection apply script), `docs/` (runbook). **No `src/` changes.**

**emp-style probe (reviewer App can create a `tester` check run):**
- [ ] If the reviewer-App installation token is available: on a scratch PR head SHA, `gh api -X POST repos/sjawhar/legion/check-runs -f name=tester -f head_sha=<sha> -f status=completed -f conclusion=success` with the App token; assert the check run is created and visible on the PR.
- [ ] If no token/scratch PR in CI: commit the exact command above into the runbook and record in the commit message: *"Deploy-time: create a `tester` check run on a scratch PR head SHA with the reviewer-App token."*

**Implementation (prepare + document, do NOT apply):**
- [ ] Commit a script `scripts/branch-protection.sh` (or `.ts`) containing the exact `gh api` call that sets `main` branch protection to require: CI + `tester` + `architect` + reviewer-App approval, with each required check **pinned to its publisher App id**. The script must be idempotent and print what it will change; it is **run manually, later**.
- [ ] Add a runbook `docs/solutions/github/branch-protection-required-checks.md` documenting: the sequencing warning (applying before the pipeline can post `tester`/`architect` blocks ALL merges, including this PR), the exact apply command, and how to pin a check to a source App.
- [ ] **Do NOT invoke the apply script against `main` in this PR.**

**Verification:**
- [ ] `bash -n scripts/branch-protection.sh` (syntax check) passes; the script is committed but unexecuted.
- [ ] Runbook renders and states the sequencing warning + exact commands.

**Task ends at verification green.** Do NOT commit — the coordinator commits the wave (see Commit protocol).

---

## Task 6 — U6 (verdict carrier via CI summary) [Wave 2, group G]

**Why (settled D26):** `tester`/`architect` check-run outcomes ride the existing debounced `pr.<n>.ci` summary. Per finding 3 (coordinator ruling), the verdict must be attributable to its **pinned publisher App** — so this task threads the check-run `app.id` into the CI-recording path and records `tester`/`architect` observations **only** when published by the pinned reviewer App. A wrong-App `tester` check run must NOT satisfy the verdict.

**Files:** `packages/envoy/internal/contracts/normalize.go` (+ `normalize_test.go`) — add publisher `app.id` to `CIObservation`; `packages/envoy/internal/webhook/github.go` (+ `github_test.go`) — pass the pinned-App filter into the CI fold; `packages/envoy/internal/webhook/config.go` (+ `config_test.go`) — **parse `ENVOY_REVIEWER_APP_ID`** into the webhook config (residual #2: this is the missing wiring; today no config field carries the pinned App id); `packages/envoy/cmd/listener/main.go` — read the parsed config value and pass it into `GitHubHandler`/the CI fold; `packages/envoy/internal/cistore/cistore.go` (+ `*_test.go`) — carry/verify publisher on `Check` only if needed for the filter; `internal/cistore/render.go` only if a payload gap is proven.

- [ ] **Payload shape (confirm first):** `internal/cistore/render.go` `RenderSummary`/`Summary` puts each check by **name** into a `StatusGroup.Checks`; conclusion is encoded by which group (`Passed`/`Failed`/`Running`/`Queued`/`Skipped`). Names+conclusions are already surfaced; the gap is **publisher identity**.
- [ ] `internal/contracts/normalize_test.go`: assert `GithubCIObservations("check_run", body)` extracts `AppID` from `check_run.app.id` (a new `CIObservation.AppID` field). Today `CIObservation` (normalize.go:75-83) has no `AppID` — this fails.
- [ ] `internal/webhook/github_test.go` **positive round-trip (emp#3)**: feed a `check_run` fixture named `tester`, `conclusion=success`, `check_run.app.id = <pinned reviewer-App id>`; assert `Record(...)` is called with `checkName="tester"`, `conclusion="success"`. Add the same for `architect`.
- [ ] `internal/webhook/github_test.go` **negative (wrong-App) test**: feed a `check_run` named `tester`, `conclusion=success`, but `check_run.app.id = <some other App>`; assert the `tester`/`architect` verdict is **NOT recorded** (either not recorded at all, or recorded under a namespaced/ignored name so it cannot satisfy the required check). A normal (non-`tester`/`architect`) CI check from any App is still recorded as today.
- [ ] `internal/cistore` render test: a `State` with `"tester": {completed, success}` renders `tester` in `Summary.Passed.Checks`; `architect=failure` renders in `Summary.Failed.Checks`; normal CI checks summarize alongside.
- [ ] Run `go test ./internal/contracts/... ./internal/webhook/... ./internal/cistore/...`; confirm failures.

**Implementation:**
- [ ] `internal/contracts/normalize.go`: add `AppID string` to `CIObservation` (struct at 75-83) and populate it from `nestedString(body, "check_run", "app", "id")` in `GithubCIObservations` (93-122). Contracts are generated — if this struct is generated from `packages/contracts/*`, add the field at the generator source and regenerate; otherwise edit `normalize.go` directly and note it.
- [ ] `internal/webhook/config.go` (+ `config_test.go`): add a `ReviewerAppID string` field parsed from `ENVOY_REVIEWER_APP_ID` (mirror the existing `ENVOY_*` parse pattern in that file; add a `config_test.go` case asserting it is read). `cmd/listener/main.go`: read it and pass it into `GitHubHandler` (which forwards to the CI fold).
- [ ] `internal/webhook/github.go` CI fold (100-114): for observations whose `CheckName` is `tester` or `architect`, record them **only** when `obs.AppID == cfg.ReviewerAppID` (the parsed pinned id). For a wrong-App `tester`/`architect`, do not record it as the bare verdict name (drop it, or record under an ignored name) so it cannot satisfy the required check. All other check names record unchanged regardless of App. If `ReviewerAppID` is unset, fail loudly at startup rather than silently accepting any App (no silent fallback).
- [ ] `internal/cistore`: `Check` (cistore.go:44-49) needs no new field if the filter happens at the fold (recording only pinned-App verdicts under the bare name). Only add a publisher field to `Check`/`Record` if a consumer must see it in the summary — keep this minimal (D26: no raw check_run publishing).
- [ ] `render.go`: change only if the render test proves a name/conclusion gap; otherwise leave untouched.

**emp#3 note:** if the live round-trip cannot run in CI, the unit tests above are the substitute; record: *"Deploy-time: post a real `tester` check run from the reviewer App and confirm it appears in the `pr.<n>.ci` summary; post one from a different App and confirm it does NOT satisfy the verdict."*

**Verification:**
- [ ] `go test ./internal/...` green (from `packages/envoy`), including the wrong-App negative test.

**Task ends at verification green.** Do NOT commit — the coordinator commits the wave (see Commit protocol).

---

## Task 7 — U7 (Daemon: artifact-driven resync/repair pass, read-only) [Wave 2, group F]

**Why:** the correctness backstop. One **read-only** pass re-derives the expected next-owner for each non-terminal issue from GitHub artifacts, cross-references live workers, and emits "artifact exists but no live owner → re-dispatch recommendation" to the controller. **The pass NEVER dispatches** — the controller executes dispatches.

**Files:** `packages/daemon/src/daemon/phase-artifacts.ts` (new — the artifact fetch contract), `packages/daemon/src/daemon/resync.ts` (new), `packages/daemon/src/daemon/__tests__/resync.test.ts` (new) + `packages/daemon/src/daemon/__tests__/phase-artifacts.test.ts` (new).

**PhaseArtifacts contract (finding 2 — artifact-derived truth, NOT label-derived).** The current `buildIssueState` derives phase facts partly from **labels** (`state/types.ts:345` area: `worker-done`/`test-passed`/etc.), but §8.4 requires the next-owner to be re-derived from **durable GitHub artifacts**. So U7 introduces an explicit artifact fetch contract, `PhaseArtifacts`, and derives from it — not from label-derived `IssueState` alone.
- [ ] `phase-artifacts.ts` exports `interface PhaseArtifacts { hasNonDraftPr: boolean; headSha: string | null; testerCheckOnHead: "success"|"failure"|null; architectCheckOnHead: "success"|"failure"|null; nativeReviewOnHead: "approved"|"changes_requested"|null; autoMergeEnabledOrMerged: boolean; merged: boolean; planHandoff: "present"|"absent"|"unresolvable"; }` plus a per-field `resolved` signal (a field is `unresolvable` when its precondition, e.g. a PR/head SHA, is absent).
- [ ] `phase-artifacts.ts` exports `fetchPhaseArtifacts(ref: IssueRef, deps): Promise<PhaseArtifacts>` where **`IssueRef` is a canonical typed reference** (residual #3), NOT a bare `issueId` string: `interface IssueRef { issueId: string; status: string; source: { owner: string; repo: string; number: number }; prRef: { owner: string; repo: string; number: number } | null }`. This avoids `parseIssueIdParts`' hyphen ambiguity (`github.ts:61` cannot losslessly split `owner-repo-number` when the owner contains hyphens); callers pass the `source`/`prRef` GitHub already gave us. Build the artifact facts on the existing query layer: `state/fetch.ts` (`getPrReviewStateBatch`, `getCiStatusBatch`), `state/github-fetch.ts` (`fetchGitHubProjectItems`), `state/backends/github.ts` (`GitHubTracker.parseIssues`). Check-run facts (`tester`/`architect` by name **and pinned publisher App**) come from the same source Task 6 records; PR draft/ready + head SHA + merged + auto-merge from the PR query. **plan handoff: mark `unresolvable`** — see the planner note below.
- [ ] **`nativeReviewOnHead` must be proven against the head SHA (finding 4).** `getPrReviewStateBatch` today queries `latestReviews(first: 1) { nodes { state } }` (fetch.ts:258-259) with **no commit OID**, so an approval on a stale commit would be mistaken for approval on the current head. Add an extended GraphQL query (either widen `getPrReviewStateBatch` or a dedicated `getReviewOnHead`) that fetches the PR `headRefOid` **and** each latest review's associated commit OID (`latestReviews { nodes { state, commit { oid } } }`). Set `nativeReviewOnHead` to the review state **only when the review's commit OID equals `headRefOid`**; on mismatch or missing OID, treat the review artifact as **absent/unresolved** (conservative — never advise/recommend off a stale approval).
- **Planner note (coordinator ruling on finding 10):** the plan-phase handoff (`legion handoff write plan`) writes `.legion/plan.json` on the **issue branch**, which does not exist until an implementer creates the PR branch — so at plan phase there is no deterministically daemon-checkable artifact. Therefore `PhaseArtifacts.planHandoff` is **always `unresolvable`**, and U7's next-owner derivation uses only the unambiguous artifacts (PR draft/ready, `tester`/`architect` check runs by name+publisher, native review on head, merged/auto-merge state). Issue-status **labels** may be used ONLY as a transitional fallback for phases with no artifact yet — **explicitly marked in a `// TODO(label-teardown): remove when labels are torn out` comment** so the label dependency is removed in the label tear-out workstream.
- Decision layer for the owner mapping: `state/decision.ts` (`ACTION_TO_MODE`), `state/types.ts` (`ActionType`, `IssueStatus.DONE`); the dispatch-implying set is the `dispatch_*`/`resume_*` values in `state/types.ts:84-108`.
- Query GitHub directly via the existing fetch layer. **Do not build a collector** and **do not add a stateful child→parent index** — use on-demand GraphQL `parent` only if a case needs it.

**Read-only enforcement (finding 7 — pin it at the type level).** Define `RunResyncDeps` to contain **only** read/fetch/status/emit callbacks: `{ listNonTerminalIssues(): IssueRef[]; fetchPhaseArtifacts(ref: IssueRef): Promise<PhaseArtifacts>; getLiveWorkers(): Promise<Record<string,…>>; getSessionStatus(sessionId): Promise<…>; emitToController(items): void }`. `listNonTerminalIssues()` returns the **canonical typed `IssueRef[]`** (issueId, status, source, prRef) so no downstream code re-parses a hyphenated id. It must **not** expose `writeStateFile`, `transitionIssue`, `removeLabel`, `createSession`, `deleteSession`, or any worker-mutation method — so a write is not even reachable from the pass. The test injects spies for all of the above-forbidden methods and asserts **none are called**.

**TDD (write failing table-driven tests first):**
- [ ] `phase-artifacts.test.ts`: given mocked GitHub responses, assert `fetchPhaseArtifacts(ref)` returns the right `PhaseArtifacts` (non-draft PR true/false, head SHA, tester/architect by name+publisher, native review on head, merged/auto-merge) and marks `planHandoff` `unresolvable`. Assert a field with no precondition (e.g. tester check when no PR exists) is reported `unresolvable`, not `false`. **Add a stale-review case (finding 4):** a review approved on an OLD commit OID (≠ `headRefOid`) → `nativeReviewOnHead` is absent/unresolved, NOT `approved`.
- [ ] `resync.test.ts`: table-driven `(PhaseArtifacts + live-worker map) → (expected recommendation set)`, consuming the **PhaseArtifacts contract** (not label-derived IssueState). Cases:
  - Non-terminal issue whose artifacts imply a dispatch-owner AND `hasLiveWorker === false` → **one recommendation** `{issueId, mode, reason: "artifact_no_live_owner"}`.
  - Same issue but `hasLiveWorker === true` → **no recommendation**.
  - Terminal issue (`IssuStatus.DONE`) → **no recommendation**.
  - Issue whose artifacts imply a non-dispatch state (e.g. waiting on a check that exists) → **no recommendation**.
- [ ] **Read-only spies (finding 7):** inject spies for `writeStateFile`/`transitionIssue`/`removeLabel`/`createSession`/`deleteSession`; assert **zero calls** to all of them inside the pass.
- [ ] Run `bun test`; confirm failure (modules not implemented).

**Implementation:**
- [ ] Create `phase-artifacts.ts` (contract + `fetchPhaseArtifacts`) per the contract above.
- [ ] Create `resync.ts` exporting a **pure** `computeResyncRecommendations(input): Recommendation[]` where `input` carries `{ issueId, status, artifacts: PhaseArtifacts, hasLiveWorker }` per non-terminal issue. Derive the expected owner-mode from the unambiguous artifacts (+ the transitional label fallback marked for teardown); if a dispatch-owner is implied and there is no live worker → push `{ issueId, mode, reason: "artifact_no_live_owner" }`.
- [ ] Export `runResyncPass(deps: RunResyncDeps)` that: (1) `listNonTerminalIssues()`, (2) `fetchPhaseArtifacts` + `getLiveWorkers`, (3) `computeResyncRecommendations`, (4) `emitToController(recs)` (default binding publishes to Envoy `notifications.legion.controller`; DI override in tests). `runResyncPass` performs **no writes and no dispatch** — enforced by `RunResyncDeps` having no such method.
- [ ] Keep `runResyncPass` side-effect-free except the injected emit. This is the seam U8 schedules and U9 extends.

**Task ends at verification green.** Do NOT commit — the coordinator commits the wave (see Commit protocol).

---

## Task 8 — U10 (Merge: adopt native auto-merge) [Wave 2, group H]

**Why (D25):** GitHub auto-merges the instant required checks + review are green; the merge worker shrinks to enabling auto-merge + handling the blocked/base-changed cases. Retro dispatch is unchanged (post-merge, merged event).

**Files:** `.opencode/skills/legion-worker/workflows/merge.md` (the direct-merge path is around **line 124**: `gh pr merge "$LEGION_ISSUE_ID" --squash --delete-branch`, under the `### 6. Merge (with conflict retry)` heading at ~119). `docs/` merge runbook note. **No daemon merge code exists** — this is a skill/doc-only change.

**emp#4 probe:**
- [ ] If the App installation token + a scratch PR are available: verify `gh pr merge --auto --squash` succeeds under the App token on the scratch PR. Confirm auto-merge is enabled (not an immediate merge unless already green).
- [ ] If not: record in the commit message: *"Deploy-time: verify `gh pr merge --auto --squash` enablement under the implementer-App installation token on a scratch PR."*

**Implementation:**
- [ ] `merge.md`: replace the direct merge at **~124** (`gh pr merge "$LEGION_ISSUE_ID" --squash --delete-branch`) with the auto-merge happy path `gh pr merge "$LEGION_ISSUE_ID" --auto --squash --delete-branch`. Remaining worker duties: handle **auto-merge-blocked** and **base-changed** cases (keep the existing classify-on-failure block at ~129-133); **no-op if already merged**. Keep the retro dispatch trigger on the merged event unchanged.
- [ ] Remove any bespoke merge-execution steps the native auto-merge replaces (boy-scout cleanup of the now-dead conflict-retry loop in the skill).

**Verification:**
- [ ] `grep -n 'gh pr merge' .opencode/skills/legion-worker/workflows/merge.md` shows the `--auto --squash` form and **no** remaining direct `gh pr merge "$LEGION_ISSUE_ID" --squash` (non-`--auto`) happy-path invocation.
- [ ] Skill/doc change reviewed for correctness against D25. No TypeScript changed (no daemon merge code exists); if any is, run `bun test` / `bunx tsc --noEmit` / `bunx biome check src/`.

**Task ends at verification green.** Do NOT commit — the coordinator commits the wave (see Commit protocol).

---

## Task 9 — U8 (Daemon: guaranteed resync trigger, I4) [Wave 3, group I]

**Why:** the resync pass (U7) needs a guaranteed trigger. Invoke it on daemon startup and on a low-frequency timer (default 10 min, configurable). **NOT** a continuous differ, **NOT** wedge-detection — read-only, output = recommendations.

**Files:** `packages/daemon/src/daemon/index.ts`, `packages/daemon/src/daemon/config.ts`, `packages/daemon/src/daemon/__tests__/index.test.ts` (extended). **U8 edits `index.ts` + `config.ts` ONLY** — it schedules `runResyncPass` from U7; it does not touch `resync.ts`.

**TDD (write failing tests first):**
- [ ] `index.test.ts` (reuse the `startDaemon` DI harness with injected `setTimeout`/fake timers, per the existing tests): assert `runResyncPass` (injected/spied) is invoked **once on startup**.
- [ ] Advance the fake timer by `resyncIntervalMs`; assert `runResyncPass` is invoked **once per tick**.
- [ ] Assert **zero writes / zero dispatch** when the pass returns no recommendations (`createSessionCalls.length === 0`).
- [ ] Run `bun test`; confirm failure.

**Implementation:**
- [ ] `config.ts`: add `resyncIntervalMs: number` to `DaemonConfig` + defaults/parse/resolve (default `600_000` = 10 min; recognized YAML key e.g. `resync_interval_seconds`; env e.g. `LEGION_RESYNC_INTERVAL_SECONDS`). Mirror the existing `checkIntervalMs`/RSS-interval config pattern (recognized-key default object ~104-120, YAML parse, env parse, resolver, output object).
- [ ] `index.ts`: inject `runResyncPass` through `DaemonDependencies`/`opts.deps` (DI, testable) with a default binding to the U7 implementation.
- [ ] `index.ts`: near the startup trigger (after `scheduleHealthTick()` at ~line 998), invoke `runResyncPass` once on startup, and add a `scheduleResyncPass` timer that mirrors `scheduleHealthTick` (uses `resolvedDeps.setTimeout`, re-schedules in a `finally` guarded by `!shuttingDown`, interval `config.resyncIntervalMs`). Clear its timeout in `shutdown` alongside `healthTickTimeout`.
- [ ] The pass output goes to the controller via the U7 `emitToController` seam. No dispatch from `index.ts`.

**Verification:**
- [ ] `bun test` green (startup-once + per-tick + zero-write).
- [ ] `bunx tsc --noEmit` clean.
- [ ] `bunx biome check src/` clean.

**Task ends at verification green.** Do NOT commit — the coordinator commits the wave (see Commit protocol).

---

## Task 10 — U9 (Daemon: idle-worker advisory) [Wave 3, group J] — THE subtle unit

**Why (exception lane §6.3):** advise the controller when a worker went idle **without producing its current-phase artifact**. Fires ONLY on an objective conjunction; **never** aborts/re-seats; **no** turn counts, **no** timeouts; when the expected artifact is ambiguous, **do not advise**.

**Files:** `packages/daemon/src/daemon/idle-advisory.ts` (new) + `packages/daemon/src/daemon/__tests__/idle-advisory.test.ts` (new); extend `packages/daemon/src/daemon/resync.ts` to invoke it inside `runResyncPass`; reuse `phase-artifacts.ts` from U7 (read-only import, not edited). **U9 edits `resync.ts` + the new files ONLY** — it does not touch `index.ts` (U8 owns that).

**Objective conjunction (both must hold to advise):**
1. The worker session emitted an **idle / turn-complete** transition — observed via the daemon's existing **pull-based** session-status observation: `adapter.getSessionStatus(sessionId)` returns `{ data: { type: "idle" (not "busy"/"retry"), lastActivityAt, ... } }` (RuntimeAdapter, `runtime/types.ts:31-32`; OpenCode impl `runtime/opencode.ts:102-161`). A worker whose current status `type === "idle"` is the idle signal. **Do not** use `turnCount`/`messageCount`/`lastActivityAt` as a wedge/timeout heuristic — they are context only; the trigger is strictly `type === "idle"`.
2. The artifact for the worker's **current expected phase** (its mode, parsed from the `{issueId}-{mode}` worker id) **does not exist**.

**Per-mode expected-artifact map — encode as DATA (a table), not branching:**

| Worker mode | Expected artifact (exists ⇒ NO advisory) |
|---|---|
| `implement` (implementer) | a non-draft PR exists for the issue |
| `test` (tester) | a `tester` check run exists on the PR's **current head SHA** |
| `review` (reviewer) | a native review exists on the PR's **current head SHA** |
| `architect` | an `architect` check run exists on the current head SHA |
| `plan` (planner) | **AMBIGUOUS — never advise** (see planner ruling below) |
| `merge` | auto-merge is enabled **or** the PR is merged |

**Ambiguity rule (encode explicitly):** if the expected artifact for a mode **cannot be resolved** (e.g. the mode needs a PR/head-SHA but none is known yet, or the mode is not in the map), the result is **AMBIGUOUS → do NOT advise**. Only a definitively-absent artifact for a resolvable expectation yields an advisory.

**Planner ruling (coordinator ruling on finding 10):** the plan-phase handoff (`legion handoff write plan`) writes `.legion/plan.json` on the **issue branch**, which does not exist until the implementer creates the PR branch — so at plan phase there is no deterministically daemon-checkable artifact (it is not on the issue as a comment, and no branch exists yet). Therefore the `plan` row is **permanently AMBIGUOUS → the advisory NEVER fires for planner mode.** Encode `plan` in `EXPECTED_ARTIFACT_BY_MODE` with an `unresolvable` resolver (not a comment-existence check), so it can never satisfy "artifact definitively absent."

**TDD — write the explicit test matrix first (`idle-advisory.test.ts`):**
- [ ] `(idle implementer, no PR)` → **one advisory**.
- [ ] `(implementer idle, non-draft PR exists)` → **no advisory** (artifact present).
- [ ] `(implementer idle, PR exists, waiting on tester)` → **no advisory** (implementer's artifact — the PR — exists; the next phase is not the implementer's concern).
- [ ] `(tester idle, tester check run present on head)` → **no advisory**.
- [ ] `(tester idle, no tester check run on head)` → **one advisory**.
- [ ] `(reviewer idle, native review present on head)` → **no advisory**; `(reviewer idle, none)` → **one advisory**.
- [ ] `(architect idle, architect check run present)` → **no advisory**; `(architect idle, none)` → **one advisory**.
- [ ] `(planner idle)` → **no advisory, always** (planner is AMBIGUOUS by ruling — assert the advisory never fires for `plan` mode regardless of any handoff state).
- [ ] `(merge idle, auto-merge enabled or PR merged)` → **no advisory**; `(merge idle, neither)` → **one advisory**.
- [ ] `(worker status "busy")` for any mode → **no advisory** (idle trigger not met).
- [ ] `(mode needs a PR but head SHA/PR unknown)` → **AMBIGUOUS → no advisory**.
- [ ] `(unknown mode not in the map)` → **AMBIGUOUS → no advisory**.
- [ ] **Assert no abort/re-seat code path exists**: grep the module for and assert absence of any `deleteSession`/abort/kill/re-seat call; the module only returns advisories.
- [ ] Run `bun test`; confirm failure (module not implemented).

**Implementation:**
- [ ] `idle-advisory.ts`: define the per-mode map as a data structure `EXPECTED_ARTIFACT_BY_MODE` (mode → an artifact-resolver descriptor); the `plan` entry resolves to `unresolvable` (never advise). Export a **pure** `computeIdleAdvisories(input): Advisory[]` taking, per running worker: `{ issueId, mode, sessionStatusType, artifacts }` where `artifacts` reuses the U7 `PhaseArtifacts` facts (hasNonDraftPr, testerCheckOnHead, reviewOnHead, architectCheckOnHead, autoMergeEnabledOrMerged, planHandoff=unresolvable, and each field's `resolved`/`unresolvable` signal). Produce exactly one advisory per worker iff `sessionStatusType === "idle"` AND the mapped artifact is **resolvable** AND definitively absent. Ambiguous (unresolvable / unmapped, including all planner cases) → no advisory.
- [ ] `resync.ts` (extend `runResyncPass`): poll `getSessionStatus` for each running worker, reuse `fetchPhaseArtifacts` (U7) for the facts, call `computeIdleAdvisories`, and hand advisories to the injected `emitToController` seam (same one U7 uses). **Never** dispatch, abort, or re-seat.
- [ ] Output = one advisory message per idle-with-missing-artifact worker to the controller.

**Verification:**
- [ ] `bun test` green (full matrix + no-abort assertion).
- [ ] `bunx tsc --noEmit` clean.
- [ ] `bunx biome check src/` clean.

**Task ends at verification green.** Do NOT commit — the coordinator commits the wave (see Commit protocol).

---

## End state

- All ten tasks landed on this jj stack via the **coordinator's per-wave commits** (see Commit protocol) — no subagent ran any version-control command — on top of the already-present spec v3 doc commits (U1).
- **ONE PR** containing every unit + the spec v3 commits. Do not split into multiple PRs.
- Branch protection is committed as a prepared+documented script, **not applied** to `main`.
- Final gate before opening the PR: from `packages/daemon` — `bun test`, `bunx tsc --noEmit`, `bunx biome check src/`; from `packages/envoy` — `go test ./...`. All green.
- Record every emp probe (emp#2, emp#3, emp#4, emp#5) that could not run in CI as an explicit deploy-time verification line in the PR description.
- The exception lane (U11) publishes to `notifications.envoy.exceptions.<original-topic>` — inside the `ENVOY_NOTIFICATIONS` stream (`notifications.>`) so it persists. The controller subscribes to `notifications.envoy.exceptions.>` for its exception lane (§6.3). Note this subscription in the controller-skill rewrite workstream (out of scope for this PR, but the topic contract is fixed here).
