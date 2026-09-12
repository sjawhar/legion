# Dispatch event fixtures

Each file is one Dispatch `Event` object (`id`, `issue_key`, `seq`, `type`, `actor`, `notify`,
`created_at`, `payload`) — exactly the shape `JSON.parse(envelope.payload)` produces on a real
`notifications.dispatch.issue.<KEY>.<type>` NATS delivery
(`packages/envoy/internal/dispatch/outbox/publisher.go:183-198` marshals the whole `Event` into
the envelope's `payload` string).

Captured 2026-09-10 against the live Dispatch server on this box (`http://localhost:8766`) and
project `LEGSMOKE` ("Legion Smoke (disposable)"), using the daemon's own actor identity
(`{kind:"session", id:"legion-daemon:LEGSMOKE"}`). Scratch issues `LEGSMOKE-1` (root),
`LEGSMOKE-2` (child), `LEGSMOKE-3` (ask host) were created, cycled through every lifecycle status,
and closed; nothing here touches the real `LEGION` project.

| File | Event | Notes |
| --- | --- | --- |
| `issue-created-root.json` | `issue.created` | Root issue, `parent: null`, `status: "triage"`. |
| `issue-created-child.json` | `issue.created` | Child issue, `parent` set to the root's key. |
| `issue-updated-todo.json` | `issue.updated` | `status: "todo"`; payload is the full issue, no `from`/`to`. |
| `issue-updated-in-progress.json` | `issue.updated` | `status: "in_progress"`. |
| `issue-updated-testing.json` | `issue.updated` | `status: "testing"`. |
| `issue-updated-needs-review.json` | `issue.updated` | `status: "needs_review"`. |
| `issue-updated-retro.json` | `issue.updated` | `status: "retro"`. |
| `issue-updated-backlog.json` | `issue.updated` | `status: "backlog"`. |
| `issue-updated-icebox.json` | `issue.updated` | `status: "icebox"`. |
| `issue-closed.json` | `issue.closed` | `status: "done"`, `closed_at` set. |
| `child-status.json` | `child.status` | Delivered to the parent; payload `{child_key, from, to}` (already has `from`/`to`, unlike `issue.updated`). |

The three `artifact.*` events the design gate consumes are **built from the published contract,
not captured**: `POST /api/v1/artifacts/{id}/reviews` is human-only (403 for an agent bearer), so
this session cannot review a document. Their payloads are the JSON in the project document "Spec
approval" (`dispatch_doc_read({ project: "LEGION", artifact: "spec-approval-design-md" })`,
section "Contract (exact shapes the daemon pins)") verbatim, wrapped in the fixtures' `Event`
envelope, with the contract's truncated ids (`5025ec5b-…`, `18258070-…`) completed to full UUIDs.

| File | Event | Notes |
| --- | --- | --- |
| `legsmoke-3-artifact.approved.json` | `artifact.approved` | Contract JSON verbatim (`version: 12`, `reason: null`, `ask_id` set — approved by answering the approval ask). Event id 241, seq 5, actor user sjawhar. This is the `design-approved` wake input. |
| `legsmoke-3-artifact.changes_requested.json` | `artifact.changes_requested` | Same shape with `reason` set (required for this type) and `ask_id: null` (given from the document header). Seq 6. This is the `design-changes-requested` wake input. |
| `legsmoke-3-artifact.version.json` | `artifact.version` | The emitter's shape (`packages/envoy/internal/dispatch/api/server.go` `versionEventPayload` + `model.Version`): `version` is an object `{number, named, summary, authors, created_at}`, not a bare integer. Unnamed version 13 written by the architect session; seq 7. Closes the gate silently after an approval at 12. |

GitHub `issues.opened` (for the "GitHub webhook events are ignored" reducer test) reuses the
existing fixture in `envelope-goldens.test.ts`; it is not a Dispatch event and needed no capture.

The issue topic carries every event (the outbox does not gate `notifications.dispatch.issue.>` on
`notify`); `notify` is the human-wake flag, and the daemon's own consumer ignores it entirely.

## Human-authored fixtures (captured 2026-09-10 02:30Z via Sami's dashboard session)

- `legsmoke-3-issue.updated-human-todo.json` — a human moving LEGSMOKE-3 from `triage` to `todo`
  through the dashboard's Status control (actor `{kind:"user", id:"sjawhar"}`); this is the
  "admit" input the daemon reacts to.
- `legsmoke-3-ask.answered-approve.json` — the human answering the former design-gate ask with
  `selected: ["Approve"]` (actor user sjawhar). Since the gate moved to document approval this is
  the negative input: `ask.answered` must leave the gate untouched.
