# Legion issue lifecycle on native Dispatch — implementation plan (T20)

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. One
> implementer per PR below, in its own jj workspace; echo the plan back before coding; every ruling
> from review rounds supersedes this document. Steps use `- [ ]` for tracking.

**Goal:** The Legion daemon derives its issue lifecycle (triage → done), human design gate, and
child issues from native Dispatch — no GitHub Projects board, no GitHub issues.

**Architecture:** A second durable JetStream consumer reads `notifications.dispatch.issue.>` and
feeds a pure `reduceDispatchEvent`; Dispatch `status` replaces board columns and the label state
machine; the daemon writes lifecycle statuses back through one small HTTP client; the architect and
controller mutate issues with the nine `dispatch_*` tools plus two daemon ops that PATCH status.
GitHub stays for PRs, CI, and the human merge review only.

**Tech stack:** TypeScript on Bun; `nats` JetStream (existing `nats-transport.ts`); zod state schema
(`legion-state.ts`); Dispatch HTTP API (`packages/envoy/internal/dispatch/api`); types from
`@legion/envoy-client/dispatch-http` (until `packages/contracts/src/dispatch-api.ts` lands with #835
— then import from there).

**Spec:** `docs/superpowers/specs/2026-09-10-legion-dispatch-lifecycle-design.md`

## Global constraints

- Dispatch statuses, verbatim: `triage icebox backlog todo in_progress testing needs_review retro done`.
- `IssueKey` is the Dispatch key, pattern `^[A-Z][A-Z0-9]*-[0-9]+$`. No `owner/repo#n` anywhere in
  `LegionState` after this plan.
- The daemon never reads or writes GitHub issues. GitHub webhook `issues.*` events are ignored.
- Daemon → Dispatch auth: `Authorization: Bearer $DISPATCH_TOKEN`, body
  `actor: {kind: "session", id: "legion-daemon:<project>"}`,
  `origin: {session_title: "Legion daemon · <project>"}`.
- Config: `dispatch_url` (base URL, no `/mcp`), `dispatch_project` (new, required), env
  `DISPATCH_TOKEN` (required when `dispatch_url` is set; never a YAML key). `board_project_ids` and
  `LEGION_BOARD_PROJECT_IDS` are rejected by name: `board_project_ids was replaced by dispatch_project`.
- Schema: `LegionState` v17 → v18 (v17 is #828's; if #828 has not merged when PR B is ready, PR B
  renumbers to v16 → v17 and #828 rebases). The migration converts a tree-less state and throws
  `Cannot migrate a Legion state with active trees to the Dispatch lifecycle` otherwise.
- PR ↔ issue linkage: branch `legion/<KEY>`; fallback the PR body line `Dispatch: <KEY>`.
- Repo rules: no barrels, `node:` imports, Biome, co-located `__tests__`, current-behaviour comments
  only, tests behavioural, no `| head`/`| tail` on gate commands.
- Coordination: PR B overlaps #828 (`legion-state.ts`, `index.ts`, `events.ts`, `api/routes`); the
  PR B implementer messages `T9Resume` via hub before touching those files and rebases after #828.

---

## PR A — daemon pane environment for Dispatch (small; independent)

**Files:** Modify `packages/daemon/src/daemon/config.ts`, `processes.ts` (root/worker/controller env
builders), `__tests__/config.test.ts`, `__tests__/processes.test.ts`, `packages/daemon/src/daemon/AGENTS.md`.

**Interfaces — produces:** `DaemonConfig.dispatchToken: string | undefined` (from env only);
`DaemonConfig.dispatchUrl` unchanged; pane env gains `DISPATCH_TOKEN`; pane env loses
`DISPATCH_MCP_URL`.

- [ ] Test: `resolveDaemonConfig` with `dispatch_url` set and no `DISPATCH_TOKEN` env throws
      `dispatch_url is set but DISPATCH_TOKEN is not; the dispatch tools would not register`.
- [ ] Test: `DISPATCH_TOKEN` set without `dispatch_url` → `dispatchToken` undefined and not exported.
- [ ] Test: root, worker, and controller launch argv contain `DISPATCH_URL` and `DISPATCH_TOKEN` and
      do not contain `DISPATCH_MCP_URL` (assert on the recorded tmux argv, as the existing
      addressing-fragment test does).
- [ ] Test: env `DISPATCH_MCP_URL` is rejected as a legacy key
      (`DISPATCH_MCP_URL was replaced by DISPATCH_URL`) — the round-4 "own alias" acceptance goes.
- [ ] Implement: read `env.DISPATCH_TOKEN` in `resolveDaemonConfig`; drop the alias acceptance
      branch; drop `DISPATCH_MCP_URL` from the three `tmuxEnv({...})` calls; add `DISPATCH_TOKEN`.
- [ ] Docs: daemon AGENTS.md "OMP invocation" section: the two env vars every pane receives.
- [ ] Gate: `bunx tsc --noEmit`; `bunx biome check --error-on-warnings src/`;
      `LEGION_E2E=1 bun test packages/daemon` — each run bare; paste summary lines.
- [ ] Real surface: start a scratch daemon with `dispatch_url` + `DISPATCH_TOKEN` (the box's
      `~/.config/opencode/envoy.json` has both), spawn one root pane with the existing smoke helper,
      and show `envoy_list`/tool list in that pane includes `dispatch_ask` (or grep the pane's OMP
      startup log for the nine tool registrations). Paste it.

## PR B — Dispatch lifecycle in the daemon (large; the core)

Commit in this order; each commit compiles and its tests pass.

### B1 — config

**Files:** `config.ts`, `__tests__/config.test.ts`.
- [ ] Test: `dispatch_project: "LEGION"` parses; missing → `dispatch_project is required`; invalid
      (`legion`, `LEGION-1`) → `dispatch_project must match ^[A-Z][A-Z0-9]*$`.
- [ ] Test: `board_project_ids` YAML key and `LEGION_BOARD_PROJECT_IDS` env → rejected by name.
- [ ] Implement `dispatchProject`; delete `boardProjectIds` and every consumer (tsc will list them —
      leave the reducer/resync compile errors for B4/B6).

### B2 — Dispatch client

**Files:** Create `packages/daemon/src/daemon/dispatch-client.ts`, `__tests__/dispatch-client.test.ts`.

**Interfaces — produces:**
```ts
export interface DispatchClient {
  listIssues(project: string): Promise<IssueSummary[]>;      // GET /api/v1/issues?project=
  getIssue(key: string): Promise<IssueDetails>;               // GET /api/v1/issues/{key}
  setStatus(key: string, status: IssueStatus): Promise<void>; // PATCH /api/v1/issues/{key}
}
export function createDispatchClient(options: {
  baseUrl: string; token: string; project: string; fetch?: typeof fetch;
}): DispatchClient;
```
Types `IssueSummary`, `IssueDetails`, `IssueStatus` come from `@legion/envoy-client/dispatch-http`.
- [ ] Test (fake fetch): `setStatus` sends `PATCH /api/v1/issues/LEGION-7` with bearer, body
      `{status, actor: {kind:"session", id:"legion-daemon:LEGION"}, origin: {session_title: "Legion daemon · LEGION"}}`.
- [ ] Test: non-2xx → throws `DispatchHttpError` with status and the server's `error` text; the
      daemon never retries inside the client (resync is the retry).
- [ ] Implement.

### B3 — state schema v18

**Files:** `legion-state.ts`, `__tests__/legion-state.test.ts`; `packages/contracts/src/legion-roles.ts`
(`IssueKey`/`formatIssueKey`/`parseIssueKey` → Dispatch keys; `roleToken` still embeds the key).

**Interfaces — produces:**
```ts
export const ISSUE_KEY_PATTERN = /^[A-Z][A-Z0-9]*-[0-9]+$/;
export type IssueStatus = "triage"|"icebox"|"backlog"|"todo"|"in_progress"|"testing"|"needs_review"|"retro"|"done";
export interface IssueNode { key: IssueKey; title: string; status: IssueStatus; parent?: IssueKey;
  children: IssueKey[]; updatedAt?: string; updatedAtSource?: "dispatch"|"resync"; }
export interface LegionState { /* … */ gates: Record<IssueKey, { designApproved?: string /* ask id */ }>; }
```
Deleted: `IssueNode.labels`, `backlogMarker`, `released`, `state`; `GateLabelSchema`;
`LegionState.prTombstones` stays; `prByBranch` keys keep `repo@branch`.
- [ ] Test: v17 state with no trees migrates to v18 (issues emptied — they were GitHub keys — with a
      console warning naming the dropped count); v17 with a tree → throws the exact message above.
- [ ] Test: strict schema rejects an `owner/repo#n` key and a `labels` field.
- [ ] Implement schema, migration, `roleToken` for Dispatch keys (`legion-<project>-LEGION-7-architect`
      — the existing encoder already replaces `/` and `#`; add a test that a Dispatch key round-trips).

### B4 — Dispatch intake (consumer + envelope decode)

**Files:** `index.ts` (second durable consumer), `events.ts` (`processDurableMessage` branch on
`topic.startsWith("notifications.dispatch.issue.")`), `__tests__/events.test.ts`, `__tests__/index.test.ts`.

**Interfaces — consumes:** `natsTransport.consumeDurable(durable, filterSubjects, handler)` as the
GitHub lane does (`nats-transport.ts:41-60`). **Produces:** `reduceDispatchEvent` input
`{type: EventType, key: IssueKey, payload: unknown, eventId: string}` decoded from the envelope
(`payload` is a JSON string of the Dispatch `Event`; `event_id` is `dispatch-<id>`).
- [ ] Test: a JetStream message on `notifications.dispatch.issue.LEGION-7.issue.updated` for project
      `LEGION` reaches the reducer; the same for key `OPS-3` is acked and ignored (project filter on
      key prefix until the `project` header lands — then prefer the header).
- [ ] Test: reducer throw after mutation → fatal (same contract as the GitHub lane); decode failure →
      term + log (poison).
- [ ] Implement: durable `legion-<project>-dispatch`, filter `["notifications.dispatch.issue.>"]`.

### B5 — reducers

**Files:** `reducers.ts` (+ new `reduce-dispatch.ts` if `reducers.ts` would exceed ~1200 lines),
`__tests__/reducers.test.ts`. Delete: `boardEvent`, `projects_v2_item` ingress, `issueEvent`'s
label/close/reopen handling for GitHub issues, `subIssue`, `SURVIVING_LABELS`, `labeledBoardIssue`.
Keep: PR/CI reducers; `issueForBranch` (pattern `^legion\/([A-Z][A-Z0-9]*-[0-9]+)$`) and a new
`issueForPrBody` (`/^Dispatch: ([A-Z][A-Z0-9]*-[0-9]+)$/m`).

**Interfaces — produces:**
```ts
export function reduceDispatchEvent(state: LegionState, event: DispatchIssueEvent, config: ReducerConfig): Effect[];
```
Mapping (each row is one test with the real payload fixture captured from a running Dispatch):
- [ ] `issue.created` root (`parent` null, status `triage`) → `IssueNode` + `{kind:"controller", payload:{type:"triage", issue}}`.
- [ ] `issue.created` child → node with `parent`, appended to parent's `children`, `child-adopted` to the parent's active role (`routeActive`).
- [ ] `issue.updated` status `→ todo` → `{kind:"admit", issue}` (new Effect kind; the executor calls `processManager.admit`).
- [ ] `issue.updated` status `→ backlog | icebox` on a root with no tree → node updated, no effect; on a root with an active tree → `{kind:"linger", tree}`.
- [ ] `issue.updated` status echo of a daemon write (`in_progress`, `testing`, `needs_review`, `retro`) → node updated, no effect.
- [ ] `issue.closed` (status `done`) → root with tree → `linger`; child → `child-closed` to the parent's active role, `children-complete` when the last open child closes.
- [ ] `child.status` on the parent → routed to the parent's active role as `{type:"child-status", child, from, to}`.
- [ ] `ask.answered` whose ask id equals `state.gates[key].designAskId` and `answer.selected` includes `Approve` → `routeActive(key, {type:"human-approved"})`; set `gates[key].designApproved = ask.id`. Any other ask → no effect.
- [ ] GitHub `issues.*` webhook events → no state change, no effect (test with today's `issues.opened` fixture).
- [ ] Stale ordering: an `issue.updated` older than `node.updatedAt` (Dispatch `updated_at`) is ignored (same rule as today's `updatedAtSource` fence).

### B6 — effects and daemon-owned status writes

**Files:** `events.ts` (effect executor: `admit`), `processes.ts` (spawnTree → `setStatus(in_progress)`;
closeTree → `setStatus(done)`), `api/routes/workers.ts` (`/phase/complete`: implement →
`testing`, test → `needs_review`, review → `retro`), `api/routes/issues.ts`.
- [ ] Test: `admit` effect calls `processManager.admit(key)`; spawnTree PATCHes `in_progress`; a
      PATCH failure is logged and does not fail the spawn (resync converges).
- [ ] Test: `/phase/complete` for `implement` PATCHes `testing`, etc.
- [ ] Routes: delete `/legion/v1/issues`, `/issues/comment`, `/issues/body`, `/issues/labels`,
      `/issues/close`, `/gates/approve`, `/admission`, `/backlog`. Add
      `POST /legion/v1/issues/status {issue, status}` — controller capability may set
      `todo|backlog|icebox` on any project issue; the architect capability may set any status on
      issues in its own tree. Re-implement `/waves/release {issues}` as `setStatus(todo)` per child
      (the resulting `issue.updated` events drive admission).
- [ ] `state.gates[key].designAskId`: the architect registers the gate ask via
      `POST /legion/v1/gates/register {issue, askId}` after `dispatch_ask`; test: unknown ask ids
      never approve.
- [ ] pi-envoy `legion` tool (`packages/pi-envoy/src/legion/tools.ts`): remove `issue_create`,
      `comment`, `post_spec`, `label_add`, `issue_close`; add `set_status {issue, status}`,
      `register_gate {issue, askId}`; keep `release_wave`, `escalate`, `spawn_worker`. Contract types
      in `packages/contracts/src/legion-daemon-api.ts` follow. Tests in `legion.test.ts`.

### B7 — resync and deletions

**Files:** `resync.ts`, `state/github-fetch.ts` (delete `fetchGitHubProjectItems`), `index.ts`,
`__tests__/resync.test.ts`.
- [ ] Test: resync lists project issues; a Dispatch status that differs from the node's → synthetic
      `issue.updated` replay through `reduceDispatchEvent` with `updatedAtSource: "resync"`.
- [ ] Test: anomalies kept: `zero-owner-tree`, `untriaged-open` (status `triage` with no controller
      wake recorded), `launch-failed`; deleted: `erroring-issue`, label reconciliation, `missed-open`.
- [ ] Test: a daemon-owned status the daemon failed to PATCH earlier (tree active, Dispatch says
      `todo`) is re-PATCHed by resync.
- [ ] Implement; delete `createBoardProjectItemsFetcher` and the GraphQL query.

### B8 — CLI and docs

**Files:** `packages/daemon/src/cli/index.ts` (delete `admit`, `backlog`, `approve`; add
`legion status <issue> <status>` calling `/issues/status` with the controller capability),
`AGENTS.md` (root), `packages/daemon/src/daemon/AGENTS.md`, `packages/daemon/src/state/AGENTS.md`.
- [ ] Update the lifecycle diagram's caption: statuses are Dispatch statuses; the label section is
      replaced by "Gates: design gate = `dispatch_ask` with `Approve`; merge gate = human PR review".
- [ ] Gate (bare commands, summary lines pasted); real surface: a scratch daemon against the local
      Dispatch server (`packages/dispatch/e2e/run-server.sh`, project `LEGSMOKE` or a fixture
      project) + a NATS container: create an issue → `triage` wake observed in the controller pane;
      `legion status <key> todo` → root pane spawns and Dispatch shows `in_progress`; close → `done`.

## PR C — skills, role prompts, smoke (after PR B's tool names are fixed; parallel with PR B)

**Files:** `skills/legion-architect/SKILL.md`, `skills/legion-controller/SKILL.md`,
`skills/legion-worker/SKILL.md`, `packages/pi-envoy/roles/*.md`, `skills/github/SKILL.md` (remove
Legion label examples), `scripts/smoke/up.sh` + `scripts/smoke/README.md` (LEGSMOKE, no board).
- [ ] Architect: design gate = `dispatch_ask({issue, question, options:[{label:"Approve"}, …]})` then
      `legion({op:"register_gate", issue, askId})`; children via `dispatch_issue({project, parent, title, spec})`;
      waves via `legion({op:"release_wave", issues})`; spec as the issue's primary artifact
      (`dispatch_artifact primary:true`); no labels anywhere; keys are Dispatch keys.
- [ ] Controller: triage decides with `legion({op:"set_status", issue, status:"todo"|"backlog"|"icebox"})`;
      the wake table loses label rows and gains `child-status`; closed-tree activity unchanged.
- [ ] Worker: `LEGION_ISSUE` is a Dispatch key; branch `legion/<KEY>`; PR body must contain
      `Dispatch: <KEY>`; questions via `dispatch_ask` (issue pre-filled).
- [ ] Drift guard test (from #824) still passes; grep `needs-approval|human-approved|legion-child|legion-backlog|legion admit|legion backlog`
      across skills/ and roles/ is empty.
- [ ] Smoke rig: `scripts/smoke/up.sh` writes `dispatch_project: LEGSMOKE`, exports `DISPATCH_TOKEN`
      from `~/.config/opencode/envoy.json`, drops `board_project_ids`.

## Acceptance (T18, after A+B+C merge)

Lifecycle smoke on `LEGSMOKE` against the live Dispatch: human creates an issue in the dashboard →
controller triages → `todo` → architect spawns, posts spec artifact, opens the `Approve` ask → Sami
answers `Approve` → planner/implementer/tester/reviewer run as separate processes → PR with
`Dispatch: LEGSMOKE-n` → human review approves → merger → `done`. Evidence: Dispatch event log for
the issue, daemon state, tmux pane list at each phase, the merged PR.
