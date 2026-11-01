# Envoy Signal Quality Implementation Plan
> **Superseded during implementation:** the owner ruled mid-flight to remove `pr.<n>.check`/`pr.<n>.ci`,
> not add `pr.<n>.merged`/`.closed`/`workflow.….branch.…`, and to publish one settled `pr.<n>.checks` event;
> the shipped surface is documented in `packages/envoy/AGENTS.md` and `skills/envoy/SKILL.md`.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every Envoy notification an agent receives carries a real one-line summary, a structured body once, the sender's identity and time, and the fields agents were fetching from GitHub by hand; subscribe/publish/send fail visibly instead of silently.

**Architecture:** Producers (Go normalizer, listener API, cistore) emit honest envelopes with additive fields; one shared TypeScript renderer (`envoy-client/src/delivery.ts`) formats them for pi-envoy and the Claude bridge, mirrored by the Go `Deliverer.Text`; the listener stamps `sender` from its registries and answers subscribe/publish with warnings and 404s instead of silent success. No new storage (the repo-seen check reads the existing JetStream stream), no new credentials, no company-specific identifiers.

**Tech Stack:** Go 1.26.1 per `packages/envoy/go.mod` (`packages/envoy`), Bun + TypeScript (`packages/contracts`, `packages/envoy-client`, `packages/pi-envoy`, `packages/claude-envoy-bridge`), zod, `@toon-format/toon`, NATS JetStream (stream + existing KV buckets), Biome, `go test`, `bun test`.

**Spec:** `docs/plans/2026-09-07-envoy-signal-quality-design.md` — Phases 1–3 minus `on_behalf_of` (trust semantics, owner decision) and Phase 4 (attachment storage, owner decision). Research inputs: `.superpowers/sdd/2026-09-07-envoy-signal-quality-plan/research-slack.md`, `.superpowers/sdd/2026-09-07-envoy-signal-quality-plan/research-sweep.md`, plan review `plan-review.md` in the same directory.

## Global Constraints

- **Public repository.** No company, private-repo, hostname, Slack team, or channel identifiers in any tracked file. Fixtures use `example-org/example-repo`, `T01234567`, `C01234567`, `example-host`.
- **jj, not git.** Commit with `jj describe -m "<type>(<scope>): <subject>"` then `jj new`. Never `git add`/`git commit`. Check `jj log -r @ --no-graph -T 'description.first_line()'` before `jj describe`: it overwrites.
- **Additive envelope only.** Every new field is optional with `omitempty` / `.optional()`. Existing consumers must parse new envelopes unchanged.
- **Renderers never emit raw bytes.** A parse failure renders the recognised fields plus `unrecognised: <list>`.
- **`payload_summary` is prose, ≤ 160 chars, one line, never JSON.** `payload` carries structure.
- **Bodies in GitHub/Slack payloads are capped at 2048 chars** with `body_truncated: true` and the URL kept.
- **No silent success.** Subscribe to a repository with no event in the stream → `warnings[]`; publish to an unheld role → 404; every `/v1` 4xx/5xx body is `{"error": string, "expected"?: string[]}` (webhook ingress handlers are out of scope).
- **No new storage or credentials.** Only the existing NATS KV buckets; no GitHub/Slack API calls from the listener.
- **Tests:** Go co-located `_test.go`; TS in `src/__tests__/` or beside the file per package convention; table-driven; deterministic. Run only the package you changed: `go test ./internal/<pkg>/...`, `bun test` inside the package. No project-wide lint/test until the coordinator's final gate.
- **Shell rules (verbatim):** (1) Never run `rm -rf` (or `rm -r`) on a path that contains a variable, `~`, or `$HOME`; `ls` the literal path first and delete that literal path. (2) Environment variables do not persist between your bash calls; set them per command (`env HOME=/tmp/x cmd`), never `export` in one call and rely on it in the next.
- **Docs travel with code.** A task that changes behaviour documented in a package `AGENTS.md`/`README.md` updates that doc in the same commit; a task that changes a tool description updates it in the same commit.

## Shared contracts (every task codes to these exactly)

### Envelope fields added (Task 1 owns the schema; Tasks 5 and 6 consume)

| JSON name | Type | Go field | Meaning |
|---|---|---|---|
| `sender` | `{session_id: string, machine?: string, cwd?: string, title?: string, roles?: string[]}` | `Sender *EnvelopeSender` | Stamped by the listener from the registry row of `source_session`. |
| `in_reply_to` | `string` (an `event_id`) | `InReplyTo string` | Message being answered. |
| `supersedes` | `string` (an `event_id`) | `Supersedes string` | Message this one replaces. |
| `urgency` | `"low" \| "med" \| "high" \| "blocking"` | `Urgency string` | Rendered only; delivery mode unchanged. |
| `expects_reply` | `"none" \| "optional" \| "required"` | `ExpectsReply string` | Rendered only. |
| `expires_at` | existing `integer` ms | existing | Deadline; rendered as `by:`. |

### Listener HTTP contract (Task 5 implements; Task 6 consumes)

- `POST /v1/messages/send` and `POST /v1/messages/publish` accept optional `in_reply_to` (string), `supersedes` (string), `urgency` (`low|med|high|blocking`), `expects_reply` (`none|optional|required`), `expires_at` (integer ms). Invalid enum → `400 {"error":"urgency must be one of low, med, high, blocking","expected":["urgency"]}`. Send response: the envelope JSON plus sibling `"recipient": "<full session id>"`. Publish response: the envelope JSON; for a `notifications.role.<role>` topic also sibling `"holder": "<session_id>"` (a holder counts only when its session is live in the session registry); no live holder → `404 {"error":"no holder for role <role>"}`.
- `GET /v1/roles/<role>` → `200 {"role": "<role>", "holder": "<session_id>", "last_seen": <ms>}` (live holder only) or `404 {"error":"no holder for role <role>"}`.
- `POST /v1/interests/subscribe` response gains optional `"warnings": ["..."]`. A failed `sessions.Put` for a session that declares a route (`port > 0 || self_subscribed`) → `503 {"error":"session registry unavailable"}`, not success.
- `POST /v1/interests/unsubscribe`: `400 {"error":"session_id is required","expected":["session_id"]}` when missing; success `200 {"removed": [...topics]}`.
- `GET /v1/sessions?dir=<substring>&title=<substring>` filters rows (case-sensitive substring); each row gains `"roles": [...]` (role names from the session's `notifications.role.<role>` interests) and `"last_seen": <ms>` (alias of `updated_at`, which stays).
- Every `/v1` 4xx/5xx and the readiness gate: `{"error": "<message>", "expected"?: ["field", ...]}`, `Content-Type: application/json`. Webhook ingress handlers (`/webhook/*`) are out of this contract.
- TypeScript client argument names (camelCase) ↔ wire (snake_case): `inReplyTo`↔`in_reply_to`, `supersedes`, `urgency`, `expectsReply`↔`expects_reply`, `expiresAt`↔`expires_at`. Tool arguments use the wire names. Client return types: `send → { envelope: Envelope; recipient: string }`, `publish → { envelope: Envelope; holder?: string }`, parsed with `EnvelopeSchema.extend({ recipient/holder: z.string().optional() })` so the sibling keys survive.

### Renderer contract (Task 6 implements; Task 7 verifies)

`packages/envoy-client/src/delivery.ts` exports `renderInbound(raw: string, sessionID: string, subject?: string): { skip: boolean; content: string; envelope?: InboundEnvelope }`. `subject` is the NATS subject the frame arrived on (pi-envoy and the Claude forwarder both have it); when `raw` is not JSON the output is `envoy: { topic: <subject ?? "unknown">, unrecognised: "payload was not JSON" }`. `skip` is true for the reader's own dispatch echo. `content` is the TOON block:

```
envoy:
  to: you (01a0…)                  # only when topic === agentSubject(sessionID)
  from: <source_session | source>  # sender.title in parentheses when present
  at: 2026-09-07T04:41:12Z         # issued_at, ISO-8601 UTC, seconds
  id: <event_id>                   # always; the value a reply passes as in_reply_to
  by: 2026-09-07T05:00:00Z         # expires_at, only when set
  urgency: high                    # only when set
  expects_reply: required          # only when set
  re: <in_reply_to>                # only when set
  supersedes: <supersedes>         # only when set
  reply_with: envoy_send(session_id="01a0…", message="...")   # source === "agent" && source_session
  reply_role: envoy_publish(topic="notifications.role.<r>", message="...")  # sender.roles[0] when present
  summary: <payload_summary>
  message: <JSON.parse(payload) | payload string>   # omitted when payload is absent or equals summary
  note: body names session <id>; the sender is <source_session>   # only when a foreign id appears
  unrecognised: source=<value>     # only when source is not a known value
```

---

## Hardening ledger

(empty — workers append `- <task>: <shortcut> — <where> — <repayment>`; the coordinator empties it before the PR gate)

## End-to-end verification plan

| Deliverable | Real surface | Driver | Substitute when the real surface needs a merge/rollout |
|---|---|---|---|
| GitHub summaries are prose; body once | A live OMP session receives a GitHub issue-comment notification | Post a comment on an issue in the public repo after listener rollout; read the rendered block in a fresh session | Task 7 driver: signed webhook fixture → branch listener → fake session → `renderInbound` output |
| Agent messages: body verbatim, `to`/`at`/`from` | `envoy send <session> "<three paragraphs>"` from a shell, received in a session | Same after `pi-legion-envoy` release and install | Task 7 driver: `POST /v1/messages/send` → fake session → `renderInbound` |
| `pr.<n>.merged`, `pr.<n>.closed`, `checks.settled`, `is_head`, `cancelled` | Merge a PR in the public repo; subscribed session sees `merged` and exactly one `checks.settled` | After rollout: open, push twice, merge a test PR | Task 7 driver: fixture sequence `pull_request opened` → `check_run` ×N → `synchronize` → `check_run` ×N → `pull_request closed(merged)`; assert topics and once-only |
| Unwired-repo warning | `envoy_subscribe` to `notifications.github.example-org.never-seen.pr.1` prints the warning | Fresh session after release | Task 7 driver: subscribe before/after a fixture for that repo |
| Role publish 404 / holder | `envoy_publish` to `notifications.role.nobody` errors; with a claimant returns holder | Fresh session after release | Task 7 driver: publish with and without `envoy_role_set` |
| Renderer tolerance | A session on the previous plugin version renders a new-field envelope without raw bytes | Send a Phase-3 envelope to a pre-release session | Task 6 unit test with an unknown `source` and unknown fields |

Acceptance status per row is recorded by the coordinator as `RAN`, `WAIVED-BY-SAMI`, or `BLOCKED` with source/dependency/image revisions.

---

### Task 1: Envelope contract — additive fields, regenerated Go

**Files:**
- Modify: `packages/contracts/src/envelope.ts`
- Modify: `packages/contracts/schemas/envelope.schema.json`
- Modify: `packages/contracts/scripts/gen-go.ts` (nested `sender` object rendering, if the generator cannot already emit a nested struct)
- Regenerate: `packages/envoy/internal/contracts/generated.go` (never hand-edit)
- Test: `packages/contracts/src/envelope.test.ts`, `packages/envoy/internal/contracts/generated_test.go` (create if absent)

**Interfaces:**
- Produces: `EnvelopeSchema` with the six fields in "Shared contracts"; Go `contracts.Envelope` gains `Sender *EnvelopeSender`, `InReplyTo`, `Supersedes`, `Urgency`, `ExpectsReply` (all `omitempty`); Go `type EnvelopeSender struct { SessionID string \`json:"session_id"\`; Machine string \`json:"machine,omitempty"\`; Cwd string \`json:"cwd,omitempty"\`; Title string \`json:"title,omitempty"\`; Roles []string \`json:"roles,omitempty"\` }`. The generator (`gen-go.ts:82-86`) currently rejects `array` and `object` envelope properties and its only nested renderer (`renderQuestionStruct`) rejects scalar arrays; this task extends it to emit an optional nested struct for an `object` property and `[]string` for an array of strings.
- `source` stays an enum in the schema (wire validation on ingress); tolerance is the renderer's job (Task 6).

- [ ] **Step 1: Failing TS test** — in `envelope.test.ts` add a case that parses an envelope carrying all six fields and one that rejects `urgency: "urgent"`. Run `cd packages/contracts && bun test src/envelope.test.ts`; expect the first to fail (unknown key stripped → assertion on `.sender` fails).
- [ ] **Step 2: Schema + zod** — add to `envelope.schema.json` `properties` and to `EnvelopeSchema`:

```ts
sender: z.object({
  session_id: z.string().min(1),
  machine: z.string().optional(),
  cwd: z.string().optional(),
  title: z.string().optional(),
  roles: z.array(z.string()).optional(),
}).optional(),
in_reply_to: z.string().min(1).optional(),
supersedes: z.string().min(1).optional(),
urgency: z.enum(["low", "med", "high", "blocking"]).optional(),
expects_reply: z.enum(["none", "optional", "required"]).optional(),
```

- [ ] **Step 3: Extend the generator, then regenerate** — in `gen-go.ts` add rendering for envelope properties of `type: object` (a named struct `Envelope<PascalCase(prop)>`, pointer field with `omitempty`) and `type: array` with `items.type: string` (`[]string`, `omitempty`). Run `bun packages/contracts/scripts/gen-go.ts` twice; the second run must produce no diff (`jj diff --stat` unchanged). Add a generator test that asserts the emitted `EnvelopeSender` declaration text and the `roles` JSON round trip. Never hand-edit `generated.go`.
- [ ] **Step 4: Go round-trip test** — `generated_test.go`: marshal an `Envelope` with every new field set, unmarshal, compare; marshal one with none set and assert none of the new JSON keys appear (omitempty).
- [ ] **Step 5: Run** — `cd packages/contracts && bun test && bunx tsc --noEmit`; `cd packages/envoy && go test ./internal/contracts/...`.
- [ ] **Step 6: Commit** — `feat(contracts): additive envelope fields — sender, in_reply_to, supersedes, urgency, expects_reply`.

---

### Task 2: Normalizer — prose summaries and actionable payloads (GitHub, Slack, Ghost Wispr)

**Files:**
- Modify: `packages/envoy/internal/contracts/normalize.go` — every function EXCEPT the CI block (`CIObservation`, `GithubCIObservations`, `GithubCIEnvelope`, `githubCIPullRequests`, `githubCIEvent`), which Task 3 owns.
- Test: `packages/envoy/internal/contracts/normalize_test.go`
- Docs: `packages/envoy/README.md` only if it documents summary text (not `packages/envoy/AGENTS.md` — Task 5 owns it).

**Interfaces:**
- Produces: `payload_summary` prose per event; `payload` JSON with the fields below; additional envelopes on `pr.<n>.merged`, `pr.<n>.closed`, `workflow.<file>.<action>.branch.<name>`.
- Consumes: nothing new. Compose topics with the existing `GithubSubject`, `GithubWorkflowSubject`, and the Go segment sanitizer already used by `GithubPushSubject` (dots → underscores). Do not add builders to `generated.go`.

**Summary formats** (one line, ≤ 160 chars; `first(s, n)` = first line of `s`, cut at `n` runes with `…`):

| Event | `payload_summary` |
|---|---|
| `issue_comment` created | `comment on <owner>/<repo>#<n> by <author>: <first(body, 100)>` |
| `issue_comment` edited/deleted | `comment <action> on <owner>/<repo>#<n> by <author>` |
| `pull_request_review_comment` | `review comment on <owner>/<repo>#<n> by <author> (<path>:<line>): <first(body, 80)>` |
| `pull_request_review` | `review <state> on <owner>/<repo>#<n> by <author>` + `: <first(body, 80)>` when body non-empty |
| `pull_request` | `pr <action>: <owner>/<repo>#<n> <first(title, 90)>`; when `closed` and `merged`: `pr merged: <owner>/<repo>#<n> by <merged_by> → <merge_sha7>` |
| `issues` | `issue <action>: <owner>/<repo>#<n> <first(title, 90)>` |
| `push` | `push to <ref_name>: <first(head_commit.message, 70)> (<after7>) by <pusher>` |
| `workflow_run` | `workflow <name> <head_branch> run <run_id> <status>/<conclusion>` (`/<conclusion>` omitted when empty) |

Every summary passes through one final `capSummary(s) string` (rune-safe, 160 max, `…` suffix) so unbounded owner/repo/workflow/branch/actor/path segments cannot exceed the limit.

**Payload fields added:** `push`: `after`, `before`, `pusher` (`pusher.name`), `head_subject`, `commit_count` (len of `commits`), `compare_url`. `pull_request`: `head_sha`, `head_ref`, `base_ref`, `merged` (`"true"`/`"false"`), `merge_commit_sha`, `merged_by` (`pull_request.merged_by.login`). `workflow_run`: `run_id`, `run_attempt`, `head_sha`, `head_branch` (added beside the existing `branch`, same value), `pr_numbers` (comma-joined from `workflow_run.pull_requests[].number`), `run_started_at`, `updated_at`. `pull_request_review_comment`: `path`, `line` (`comment.line`, else `original_line`). All events: `body` capped by `capBody(s) → (string, truncated bool)` at 2048 runes; when truncated add `body_truncated: "true"`. `issue_comment`/`pull_request_review_comment` with `action == "edited"`: omit `body`, set `body_changed: "true"`.

**Additive topics** (append to `GithubEnvelopes` following the mention fan-out: most specific copy FIRST, same dedupe key):
- `pull_request` `closed` + `merged` → copy on `GithubSubject(owner, repo, "pr."+num+".merged")`; `closed` + not merged → `"pr."+num+".closed"`.
- `workflow_run` → copy on `GithubWorkflowSubject(owner, repo, file, action) + ".branch." + sanitize(head_branch)` when `head_branch != ""`.

**Slack** (`SlackEnvelope`, `slackSummary`, new `slackPayload`; findings F1–F3, F6, F7 of `research-slack.md`). Today `payload_summary` is a JSON string and `payload` is unset, so agents get an ID-heavy blob and no structure. Set both:

- `slackKind` stays `mention` for `app_mention`, else `message` (topics unchanged). Read `event.subtype`; for `message_changed` the message facts live under `event.message` (`text`, `user`, `ts`, `edited.user`); for `message_deleted` use `event.deleted_ts` and no text; for `bot_message` the sender label is `event.username`, else `event.bot_profile.name`, else `event.bot_id`; for `thread_broadcast` the parent is `event.root.ts`.
- `payload_summary` (IDs, since names need the Web API): `slack mention in <channel> from <user>: <first(text, 100)>`; `slack message in <channel> from <user>: <first(text, 100)>`; thread reply (`thread_ts` set, no subtype): `slack thread reply in <channel> from <user> (thread <thread_ts>): <first(text, 100)>`; `bot_message`: `slack bot message in <channel> from <label>: <first(text, 100)>`; `message_changed`: `slack message edited in <channel> by <user> at <message.ts>: <first(new text, 100)>`; `message_deleted`: `slack message deleted in <channel>: <deleted_ts>`; files present: append ` (<N> file(s): <first title or filetype>)`; unknown subtype: `slack <subtype> message in <channel>: <first(text, 100)>`.
- `payload` JSON (strings; omit empty): `kind`, `event_type`, `subtype`, `team_id`, `channel_id`, `channel_type`, `user_id`, `bot_id`, `bot_name`, `ts`, `event_ts`, `thread_ts`, `root_ts`, `text` (capped 2048, `body_truncated: "true"` when cut), `edited_by`, `deleted_ts`, `file_count`, `files` (comma-joined `name|filetype` pairs, ≤ 10), `attachment_count`.
- Slack test fixtures use `T01234567` / `C01234567` / `U01234567` / `B01234567` (replace the realistic-looking ids in `normalize_test.go` and `webhook/slack_test.go`).

**Ghost Wispr** (F9): same split. `payload_summary`: `ghostwispr <event_type> for session <session_id>` + `: <first(title, 80)>` when a title exists. `payload` keys (strings, omit empty): `event_type`, `session_id`, `title`, `duration`, `created_at`; for `summary_ready` additionally `status`, `summary` (capped 2048 with `body_truncated`), `summary_preset`, `timestamp`, `version`, `payload_type` (the body's `payload.type`). Tests: `TestGhostWisprSummary` and `TestGhostWisprPayload` for `session_started`, `session_ended`, `summary_ready` (one with a 3000-char summary).

`workflow_run` payload keeps the existing `branch` key AND adds `head_branch` (equal values) — additive contract; a test asserts both.

Out of scope here: channel/user display names, permalinks, reaction aggregates (Web API), any Slack app subscription manifest (F5), and `packages/envoy/AGENTS.md` (Task 5 owns that file; this task's new topics are listed in Task 5's docs step).

- [ ] **Step 1: Failing tests** — table-driven `TestGithubSummary` (one row per event above, fixtures under `example-org/example-repo`), `TestGithubPayloadFields` (push, pull_request merged, workflow_run with two PRs, edited comment omits body, 3000-char body truncated), `TestGithubEnvelopesMergedTopic` (merged copy first, same dedupe key; closed-unmerged → `.closed`), `TestGithubEnvelopesWorkflowBranchTopic`, plus the Slack equivalents from the report. Run `cd packages/envoy && go test ./internal/contracts/ -run 'TestGithub|TestSlack'`; expect failures.
- [ ] **Step 2: Implement** — replace `summaryJSON` use in `githubSummary` with the formats above (keep `summaryJSON` for `githubPayload`); add `capBody`; add the fields; add the fan-out copies; Slack per report.
- [ ] **Step 3: Run** — `go test ./internal/contracts/...`; `gofmt -l internal/contracts` prints nothing.
- [ ] **Step 4: Docs** — `packages/envoy/README.md` only if it describes summary text; nothing in `packages/envoy/AGENTS.md`.
- [ ] **Step 5: Commit** — `feat(envoy): prose summaries, actionable payload fields, merged/closed and branch-scoped topics`.

---

### Task 3: cistore — cancelled bucket, latest attempt, head tracking, `checks.settled`

**Files:**
- Modify: `packages/envoy/internal/cistore/cistore.go`, `render.go`, `loop.go`
- Modify: `packages/envoy/internal/contracts/normalize.go` — ONLY the CI block: `CIObservation` gains `CheckRunID string` (from `check_run.id` via `nestedNumberString`) and `URL string` (from `check_run.html_url`); nothing else in that file.
- Modify: `packages/envoy/internal/webhook/github.go` — pass `o.CheckRunID`, `o.URL` to `Record`; on `pull_request` `opened|synchronize|reopened` call `ci.RecordHead(owner, repo, number, head_sha)`; `packages/envoy/internal/webhook/webhook.go` — `CIRecorder` interface gains `RecordHead`; `CIRecorderFunc` becomes a two-method adapter struct (`CIRecorderFuncs{Record, RecordHead}`) and every mock in `webhook_test.go` is updated; `packages/envoy/cmd/listener/main.go:363-364` — the construction site of that adapter (this is the ONLY line range in `cmd/listener` this task touches; Task 5 owns the rest of `main.go`).
- Test: `cistore_test.go`, `render_test.go`, `loop_test.go`, new `packages/envoy/internal/contracts/ci_observations_test.go` (do not edit `normalize_test.go`), `webhook/github_test.go`, `webhook/webhook_test.go`.

**Interfaces:**
- Produces: `Store.Record(owner, repo, number, sha, checkName, checkRunID, url, status, conclusion string) error`; `Store.RecordHead(owner, repo, number, sha string) error`; `Store.Head(owner, repo, number string) (string, bool)`; `Check.CheckRunID string`, `Check.URL string`; `Summary.Cancelled StatusGroup`, `Summary.IsHead bool`, `Summary.FailingChecks []struct{Name, URL string}` (`json:"failing_checks"`); new topic `pr.<n>.checks.settled`.

**Behaviour:**
- `classify`: `cancelled` → `catCancelled` (new); JSON bucket `"cancelled"`.
- `Record` ignores an observation whose `CheckRunID` parses to a number lower than the stored `Check.CheckRunID` for the same name (a re-run creates a new, higher check_run id; late events from the previous attempt must not overwrite).
- Heads live in the same KV bucket under key `head.<owner>.<repo>.<number>` (value: sha string); `watch()` skips keys with the `head.` prefix for the State cache and maintains a `heads map[string]string`. While in `watch()`: a malformed value logs `WARN` with key and revision and evicts the cached entry (sweep F10; Task 4 does the same for the interest and session watchers).
- `runSummaryTick`: `RenderSummary` sets `IsHead = (sha == head)`; when a head is known and `!IsHead`, skip the state entirely (no publish, no MarkEmitted). When no head is known (PR opened before this release), treat as head.
- Settled: after a `ci` summary is published for a head state with `Running.Count == 0 && Queued.Count == 0 && len(Checks) > 0`, publish one envelope to `GithubSubject(owner, repo, "pr."+n+".checks.settled")`, summary `checks settled on <owner>/<repo>#<n> @ <sha7>: <passed> passed, <failed> failed, <cancelled> cancelled, <skipped> skipped` (+ `; failing: a, b` when failed > 0), payload = the `Summary` JSON with `kind: "checks_settled"` and `failing_checks: [{name, url}]`. Emit-once via `State.SettledEmitted bool` set with a CAS helper `MarkSettled(key) (bool, error)` mirroring `MarkEmitted`.

- [ ] **Step 1: Failing tests** — `render_test.go`: cancelled bucket; `IsHead` true/false. `cistore_test.go` (existing embedded-NATS harness): stale check_run id ignored; `RecordHead`/`Head`. `loop_test.go`: non-head state not published; settled fires exactly once for a head after all checks complete, not again on a no-op tick, and again for a new head sha. `ci_observations_test.go`: `CheckRunID` extracted. `github_test.go`: `pull_request synchronize` calls `RecordHead`. Run `go test ./internal/cistore/... ./internal/contracts/ -run 'CI|Check' ./internal/webhook/...`; expect failures.
- [ ] **Step 2: Implement** as specified.
- [ ] **Step 3: Run** the same packages; `gofmt -l` clean.
- [ ] **Step 4: Docs** — `docs/solutions/envoy/*ci-summary*.md` if it describes buckets/emission. Not `packages/envoy/AGENTS.md` (Task 5 owns it).
- [ ] **Step 5: Commit** — `feat(envoy): CI summaries track the PR head and latest attempt, bucket cancelled, emit checks.settled once`.

---

### Task 4: Interest registry correctness — unsubscribe write-through, role-claim ordering, malformed-value logging

**Files:**
- Modify: `packages/envoy/internal/store/kv.go`
- Test: `packages/envoy/internal/store/kv_test.go` (existing embedded-NATS harness)
- Docs: `docs/solutions/envoy/` — add `unsubscribe-resurrection.md` (one page: symptom, mechanism, fix) following the existing files' shape.

**Interfaces:** none new; behaviour of `Registry.Remove`, `Registry.SetRole`, and the KV watchers in `store/kv.go` and `session/registry.go` (also owned by this task; `cistore` watcher is Task 3's).

**Behaviour** (research-sweep F1, F10, F11):
- `Registry.Remove(sessionID, topics)` writes through to `r.cache` after the KV mutation succeeds: partial removal → `r.cache[sessionID] = item`; empty `topics` or zero remaining topics → `delete(r.cache, sessionID)` (mirror `SessionRegistry.Delete` / `Put` in `internal/session/registry.go`). Today the next heartbeat `Upsert` merges the stale cached topics back, resurrecting an unsubscribed topic — the mechanism behind "unsubscribed but still receiving" reports.
- Role claim (`SetRole`) is atomic. Current order is: remove the role topic from the old holder's interest → `Upsert` the new holder's interest → `roleKV.Put`; a failure at the last step leaves the new session advertising a role it does not hold, and a failure at the second step leaves the old holder stripped while still recorded in `roleKV`. New order with compensation: (1) read the current holder `old`; (2) `Upsert` the new holder's interest with the role topic; (3) CAS-write `roleKV[role] = sessionID` (`Update` with the revision read in step 1, or `Create` when there was none) — on failure, `Remove` the role topic from the new holder's interest and return the error; (4) if `old != "" && old != sessionID`, `Remove` the role topic from `old`'s interest — on failure log `WARN` and return the error, but do not undo steps 2–3 (the authoritative `roleKV` already names the new holder; the stale topic on `old` is harmless because delivery resolves the holder from `roleKV`). Tests inject failure at steps 2, 3, and 4 and assert `roleKV` and both interests end in the specified state for each.
- Watchers in `store/kv.go` and `session/registry.go`: a malformed value logs `WARN` with key and revision and evicts the cached entry, so a bad write cannot leave a stale route live; tests prove the route disappears (`Match` no longer returns the session), not only that a log line exists.

- [ ] **Step 1: Failing tests** — `TestRemoveWritesThroughCache`: subscribe `a`,`b`; `Remove(b)`; immediately `Upsert` with `[a]` (simulating the heartbeat); `Get` and `Match` must not contain `b`. `TestRemoveAllClearsCache`. `TestSetRoleRollsBackWhenInterestUpsertFails` and `TestSetRoleLeavesInterestWhenRoleWriteFails`: inject failure at each write; assert both role KV and interests equal their prior state. `TestWatcherEvictsMalformedValue` (interest and session watchers): put invalid JSON; assert a WARN line names key and revision AND `Match`/`Get` no longer return the entry. Run `cd packages/envoy && go test ./internal/store/... ./internal/session/...`; expect failures.
- [ ] **Step 2: Implement.** — [ ] **Step 3: Run** `go test ./internal/store/...`; `gofmt -l` clean. — [ ] **Step 4: Docs** (the solutions page). — [ ] **Step 5: Commit** — `fix(envoy): unsubscribe writes through the interest cache; role claims are atomic; malformed KV values are logged`.

---

### Task 5: Listener API — honest send/publish, sender stamp, role answers, unwired-repo warning, error bodies

**Files:**
- Modify: `packages/envoy/cmd/listener/api.go`, `packages/envoy/cmd/listener/main.go` (readiness-gate JSON error; NOT lines 363-364, which Task 3 owns), `packages/envoy/internal/session/session.go` (`Deliverer.Text`)
- Test: `api_test.go`, `session_test.go`
- Docs: `packages/envoy/AGENTS.md` (this task is the sole owner: API table AND the topic list, which gains `pr.<n>.merged`, `pr.<n>.closed`, `pr.<n>.checks.settled`, `workflow.<file>.<action>.branch.<name>` from Tasks 2–3), `packages/envoy/deploy/README.md` if it lists API routes.

**Depends on:** Task 1 (`generated.go` fields).

**Behaviour:**
- Send/publish: `payload_summary = firstLine(message, 160)`; `payload = message` when `message != payload_summary`; the request may still supply `payload` on publish, which wins. Pass through `in_reply_to`, `supersedes`, `urgency` (validate enum → 400 with `expected`), `expects_reply` (same), `expires_at`.
- `sender` stamp: when `source_session` is set: interest registry row → `machine`, `cwd` (`Dir`), `roles` (role names parsed from topics with the `notifications.role.` prefix); session registry row → `title`. Each lookup that fails simply omits its fields; the send never fails on a missing row. Test: two roles + missing session row.
- Role topics on publish: holder = `store.Registry.RoleHolder(role)` AND `isSessionLive(sessions, holder)`; otherwise 404 as contracted; on success publish and add `"holder"` to the response.
- `GET /v1/roles/<role>` as contracted (same liveness rule).
- Unwired-repo warning on subscribe: for each topic with prefix `notifications.github.<owner>.<repo>.`, query the existing JetStream stream with `js.StreamInfo(streamName, &nats.StreamInfoRequest{SubjectsFilter: "notifications.github.<owner>.<repo>.>"})` (nats.go legacy JetStream API) and read `info.State.Subjects`; when the map is empty, append `no GitHub event for <owner>/<repo> in the stream's retention window; is the App installed there?` to `warnings`. No new bucket, no write path. The stream name is the one the listener's consumer binds to in `cmd/listener/main.go`; expose it and the `JetStreamContext` through the handler deps. If the query errors, log WARN and omit the warning.
- All `http.Error(... 4xx/5xx)` calls in `api.go` and the readiness gate in `main.go` become `writeJSONError` with `expected` where a field is missing (research-sweep F3 lists every site). `/v1/interests/unsubscribe` validates `session_id` (400 with `expected: ["session_id"]`) and returns JSON `{"removed": [...]}` instead of `ok`.
- Subscribe: when `body.Port > 0 || body.SelfSubscribed` and `sessions.Put` fails, return 503 JSON `{"error": "session registry unavailable"}` instead of reporting the interest as success (F4).
- `Deliverer.Text` prints the same fields as the TS renderer (`to`, `from`, `at`, `by`, `urgency`, `expects_reply`, `re`, `supersedes`, reply hints, summary, body once) in the existing bracket-header text style.
- `GET /v1/sessions`: `dir`/`title` substring filters; `roles` per row.

- [ ] **Step 1: Failing tests** for each bullet in `api_test.go` (httptest, fake registries, a fake `StreamInfo` seam), `session_test.go` (Text shape, body once).
- [ ] **Step 2: Implement.** — [ ] **Step 3: Run** `go test ./cmd/listener/... ./internal/session/...`; `gofmt -l` clean. — [ ] **Step 4: Docs.** — [ ] **Step 5: Commit** — `feat(envoy): sender stamp, role 404/holder, unwired-repo warning, first-line summaries, JSON error bodies`.

---

### Task 6: Shared TS renderer, client, pi-envoy, Claude bridge

**Files:**
- Modify: `packages/envoy-client/src/delivery.ts` (becomes the renderer), `package.json` (add runtime dependency `@toon-format/toon`), `transport.ts` (new send/publish args and result types, `getRole`, `listSessions` filters, retry), `tool-contract.ts` (arguments for the five new fields; descriptions with the complete topic guide: `agent.<session_id>` (subscribe: own inbox), `role.<role>` (publish-to; holders claim via `envoy_role_set`), GitHub `pr.<n>`, `pr.<n>.check`, `pr.<n>.ci`, `pr.<n>.checks.settled`, `pr.<n>.merged`, `pr.<n>.closed`, `pr.<n>.review`, `pr.<n>.comment`, `pr.<n>.mention`, `issue.<n>`, `issue.<n>.comment`, `issue.<n>.mention`, `mention`, `push.branch.<name>`, `push.tag.<name>`, `workflow.<file>.<action>`, `workflow.<file>.<action>.branch.<name>`, Slack `slack.<team>.<channel>.message|mention`, `slack.<team>.<channel>.thread.<ts>.message|mention`, `ghostwispr.<session>.<kind>`, `whatsapp.<phone>.<jid>.<kind>`, `envoy.exceptions.<original-topic>`), new operations `envoy_role_get` (all hosts) and `envoy_inbox` (Pi only)
- Modify: `packages/pi-envoy/extensions/envoy.ts` (call `renderInbound(raw, sessionID, subject)`; ring buffer of 50 for `envoy_inbox`; print subscribe `warnings`; register both operations), `packages/claude-envoy-bridge/src/envoy-monitor.ts` (call `renderInbound(raw, sessionID, subject)`), `packages/claude-envoy-bridge/src/envoy-mcp-server.ts` (add the `envoy_role_get` dispatcher case; exclude `envoy_inbox` from the Claude tool list; manual `envoy_subscribe` with no NATS forwarder returns an error naming `ENVOY_NATS_URL` instead of recording an undeliverable interest; dispatch auto-subscribe stays best-effort — research-sweep F5)
- Test: `packages/envoy-client/src/__tests__/delivery.test.ts`, `transport.test.ts`, `tool-contract.test.ts`; `packages/pi-envoy/extensions/envoy.test.ts`; `packages/claude-envoy-bridge/tests/envoy-monitor.test.ts`
- Docs: `packages/pi-envoy/AGENTS.md`, `packages/pi-envoy/README.md`, `packages/claude-envoy-bridge/README.md`, `packages/envoy-client/README.md` where they describe rendering or tools.

**Depends on:** Task 1 (types). Codes to the Task 5 HTTP contract without waiting for it.

**Behaviour:**
- `renderInbound` per the Renderer contract, `id` line always present. Parse with a lenient local schema (`source: z.string()`, `.passthrough()`), never `EnvelopeSchema` strict; on non-JSON input render `envoy: { topic: subject ?? "unknown", unrecognised: "payload was not JSON" }` — never the raw string.
- Foreign-session note: regex `/\b01a0[0-9a-f]{4}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}\b/g` over `payload_summary` + `payload`; any match ≠ `source_session` and ≠ `sessionID` → `note`.
- `message` omitted when `payload` is undefined or equals `payload_summary`.
- Retry: one retry after 250 ms on 5xx or `fetch` network error, then throw with the server's `error` text (and `expected` when present).
- `envoy_inbox` (Pi only): last 50 rendered deliveries `[{event_id, at, from, summary}]` newest first.
- Send result renders `sent <event_id> to <recipient>`; publish result to a role renders `published <event_id>; holder <session_id>`.
- Tool descriptions state the delivery contract in one sentence: at-least-once, possibly out of order across topics; use `id` for dedupe and `at` for freshness (research-sweep F8).

- [ ] **Step 1: Failing tests** — renderer: GitHub comment (summary prose + message once), agent three-paragraph message (`to`, `from`, `at`, no `summary` duplication), unknown source + unknown fields render without raw bytes, foreign-id note, own dispatch echo → `skip`, `reply_role` when `sender.roles` present. Transport: retry once on 503 then success; 400 error surfaces `expected`. pi-envoy: subscribe warning surfaced in tool result; `envoy_inbox` returns the last deliveries. Bridge: uses the shared renderer (same output as pi-envoy for one fixture).
- [ ] **Step 2: Implement.** — [ ] **Step 3: Run** `bun test` and `bunx tsc --noEmit` in each of the four packages; `bun run lint` in each. — [ ] **Step 4: Docs.** — [ ] **Step 5: Commit** — `feat(envoy-client): one renderer for every host — to/from/at, body once, tolerant parse, inbox, role lookup`.

---

### Task 7: Local end-to-end driver and acceptance

**Files:**
- Create: `packages/envoy/scripts/e2e-local.sh` (+ `e2e-local.test.sh` smoke of the script's argument handling, matching `scripts/smoke/*.test.sh` style)
- Create: `packages/envoy/scripts/fixtures/github/*.json` (issue_comment created/edited, pull_request opened/synchronize/closed-merged, check_run ×3 incl. cancelled, workflow_run with `pull_requests`, push) under `example-org/example-repo`
- Docs: `packages/envoy/README.md` "Local end-to-end" section.

**Driver contract:** `e2e-local.sh` (1) starts `nats-server -js` in Docker (`--name envoy-e2e-nats`, host port from `${E2E_NATS_PORT:-14222}`); (2) builds `./cmd/listener` and starts it with exactly: `PORT=${E2E_PORT:-19020} ENVOY_MACHINE_ID=e2e-local NATS_URLS=nats://127.0.0.1:${E2E_NATS_PORT:-14222} ENVOY_WEBHOOKS=github ENVOY_GITHUB_WEBHOOK_SECRET=e2e-local ENVOY_REVIEWER_APP_ID=1` (any further required variable per `internal/config/config.go` and `internal/webhook/config.go` is added with a harmless value and documented in the script header); (3) waits for `GET /healthz` = 200; (4) starts a Bun fake session (HTTP server logging every `prompt_async` body to `out/e2e/session-prompts.jsonl`) and registers it via `/v1/interests/subscribe` with the topics under test; (5) starts a Bun NATS capture subscriber on `notifications.>` writing raw frames to `out/e2e/envelopes.jsonl`; (6) for each fixture POSTs an HMAC-signed webhook (`X-Hub-Signature-256`) and, for direct sends, `POST /v1/messages/send`; (7) renders every captured raw envelope with `renderInbound` into `out/e2e/rendered-ts.txt` (proves the TypeScript renderer) and prints the fake session's received prompt texts into `out/e2e/rendered-go.txt` (proves `Deliverer.Text`); (8) prints a table `topic → summary` and exits non-zero if any expected topic is missing or `checks.settled` appears more than once per sha; (9) stops and removes `envoy-e2e-nats` by literal name.

- [ ] Steps: write the fixtures; write the script; run it against the integrated branch; commit `test(envoy): local end-to-end driver for envelope rendering and CI settlement`. The acceptance agent then runs it and records observed output for every row of the verification plan.

---

### Task 8: Envoy skill — guidance that matches the new defaults

**Files:**
- Modify: `skills/envoy/SKILL.md` (sole owner)
- Modify: `skills/dispatch/SKILL.md` only where it names Envoy topics or tools

**Interfaces:** consumes the topic set and rendered shape produced by Tasks 2, 3, 5, 6 (see the owner rulings in the ledger): under `pr.<n>.>` exactly `pr.<n>` (lifecycle; `closed` carries `merged`, `merge_commit_sha`, `merged_by`, `head_sha`), `pr.<n>.comment`, `pr.<n>.review`, `pr.<n>.mention`, `pr.<n>.checks` (one event per settled head; re-fires with `superseded_settlement`); `workflow.<file>.<action>` only for runs with no associated PR; rendered block fields `to/from/at/id/by/urgency/expects_reply/re/supersedes/reply_with/reply_role/summary/message/note`; tools `envoy_send` (returns `event_id` + `recipient`; args `in_reply_to`, `supersedes`, `urgency`, `expects_reply`, `expires_at`), `envoy_publish` (role → `holder` or error), `envoy_role_get`, `envoy_inbox` (Pi only), subscribe `warnings`.

**Behaviour — the skill teaches defaults, not menus:**
- Opening section "The one subscription you need for a PR": `notifications.github.<owner>.<repo>.pr.<n>.>` and what arrives on it, in order, for a typical push (synchronize → comments/reviews → one `checks`). State plainly that `pr.<n>.check`, `pr.<n>.ci`, `pr.<n>.merged`, `pr.<n>.closed` no longer exist and why.
- "How to read a notification": one annotated rendered block; `to: you` means your inbox; `from` + `reply_with` is the reply target — never a session id quoted in the body (the misrouting incident, described without identifiers); `at` for freshness; `id` for `in_reply_to`.
- "Talking to another session": use `in_reply_to` for every answer; `expects_reply: none` for FYIs; `urgency` only when true; put the artefact URL in the message, not the prose; never address a session by tmux pane.
- "Waiting for CI or a merge": subscribe to `pr.<n>.>`, then end the turn — `pr.<n>.checks` wakes you when the head settles, `pr.<n>` `closed` with `merged: true` tells you it merged. No `gh` pollers. Explain re-arm (`superseded_settlement`).
- "When a subscription is silent": the `warnings` on subscribe; `envoy_list`; the App must be installed on the repository (generic wording, no private repo names).
- "Roles": publish to a role returns the holder or an error; `envoy_role_get` to find one.
- Keep the Slack, Ghost Wispr, WhatsApp, Legion role-token, and exception-lane sections but update Slack to describe prose summaries and structured payloads (subtype, thread, bot label).
- Delete stale patterns and the WhatsApp synthetic smoke-test section if it no longer reflects the runtime (verify against packages/envoy; if still accurate, keep it, shortened).
- No private identifiers: `example-org/example-repo`, `T01234567`, `C01234567`.

- [ ] Steps: read the four task reports for the exact produced strings; rewrite; verify every topic named in the skill exists in `packages/contracts/src/subject.ts` or the normalizer at the integrated head and every tool named exists in `packages/envoy-client/src/tool-contract.ts`; commit `docs(envoy): skill teaches the pr.<n>.> default, the rendered block, and reply etiquette`.

---

## Self-review (coordinator)

- Spec coverage: Phase 1 → Tasks 2, 5, 6; Phase 2 → Tasks 2, 3; Phase 3 minus `on_behalf_of` → Tasks 1, 5, 6; unwired repo, role 404, retry, error bodies → Tasks 5, 6; inbox → Task 6; discoverability → Task 6; Slack/Ghost Wispr → Task 2 (research-slack F1–F3, F6, F7, F9); sweep F1/F10/F11 → Task 4; F2/F3/F4/F6 → Task 5; F5/F7/F9/F8-doc → Task 6.
- Type consistency: `CheckRunID string` (Task 3) matches `nestedNumberString` output; `EnvelopeSender` field names match the JSON table; `renderInbound` signature identical in Tasks 6 and 7.
- Not done, with reasons: persistent cross-restart dedupe (sweep F8) needs a new KV bucket — storage decision for the owner; Slack display names/permalinks and GitHub review inline comments need API credentials; `on_behalf_of` and attachments per the spec §9.
- Ownership check: `normalize.go` — Task 2 (all non-CI functions) and Task 3 (CI block only); `normalize_test.go` — Task 2 only; `store/kv.go`, `session/registry.go` — Task 4 only; `cistore/*`, `webhook/github.go`, `webhook/webhook.go`, `cmd/listener/main.go:363-364` — Task 3 only; `api.go`, the rest of `main.go`, `session/session.go`, `packages/envoy/AGENTS.md` — Task 5 only; all TS packages — Task 6 only; `contracts/*`, `generated.go` — Task 1 only; `packages/envoy/scripts/*` — Task 7 only.
