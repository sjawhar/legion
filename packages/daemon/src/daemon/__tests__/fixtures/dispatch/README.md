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
| `ask-opened.json` | `ask.opened` | Options `[{label:"Approve"}, {label:"Reject"}]`, matching the architect's design-gate ask shape. |
| `ask-answered.json` | `ask.answered` | **Reconstructed, not a raw capture**: `POST /asks/{id}/answer` is human-only (403 for an agent bearer), so this session cannot answer its own ask. Built from `ask-opened.json` (real) with an `answer` object copied field-for-field from a genuine selection-based `ask.answered` read via `GET /api/v1/issues/LEGION-4/events` (event id 134, read-only, LEGION project) — only the selected option (`"Approve"`) and answering actor are substituted to match this fixture's own ask. |

GitHub `issues.opened` (for the "GitHub webhook events are ignored" reducer test) reuses the
existing fixture in `envelope-goldens.test.ts`; it is not a Dispatch event and needed no capture.

The issue topic carries every event (the outbox does not gate `notifications.dispatch.issue.>` on
`notify`); `notify` is the human-wake flag, and the daemon's own consumer ignores it entirely.

## Human-authored fixtures (captured 2026-09-10 02:30Z via Sami's dashboard session)

- `legsmoke-3-issue.updated-human-todo.json` — a human moving LEGSMOKE-3 from `triage` to `todo`
  through the dashboard's Status control (actor `{kind:"user", id:"sjawhar"}`); this is the
  "admit" input the daemon reacts to.
- `legsmoke-3-ask.answered-approve.json` — the human answering the design-gate ask with
  `selected: ["Approve"]` (actor user sjawhar); this is the `design-approved` wake input.
