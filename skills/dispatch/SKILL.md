---
name: dispatch
description: Use when you need a human decision from Sami or the user — asking a question, waiting on approval, saying "let me know", writing an end-of-message question block, picking between options with tradeoffs, or starting unattended/long-running work that will need input later. Raises the question as a durable GitHub-issue thread instead of a transcript question nobody will see.
---

# Dispatch

`dispatch` turns a question into a durable, dashboard-visible GitHub-issue thread instead
of a line buried in a transcript. Use it any time you would otherwise leave a question for
a human to notice on their own.

## When to dispatch

- Any question not answerable at the keyboard in seconds.
- Anything decision-shaped: a fork in approach, a tradeoff only the human can weigh, an
  approval gate.
- Anything you would otherwise write as an end-of-message question block and hope someone
  reads.
- Before you park on a blocker. Never leave a session idle on an unasked question.
- Unattended or long-running work (Legion roles, background agents) that will need input
  later — dispatch the question as soon as you know you'll need it, don't wait to be asked.

## When NOT to dispatch

- A one-word clarification the human is actively typing with you right now, in the same
  turn — use the built-in `ask` tool instead. `dispatch` is for questions the reader has
  not seen your transcript for; `ask` is for questions inside a live conversation.
- Anything you can resolve yourself from tools or repo context. Dispatch is for genuine
  human decisions, not a substitute for research.

## How

```
dispatch({
  subject:  "Postgres migration: run online or take a maintenance window?",
  context:  "Implementing the users-table index migration for issue #482. The table has
             40M rows; a plain CREATE INDEX locks writes for ~6 minutes in the staging
             timing test I just ran.",
  question: "Current: no online-migration tooling wired into this repo's deploy pipeline.
             Desired: index added without a customer-visible write outage.
             Option A — CREATE INDEX CONCURRENTLY, no lock, ~25 min, can't run inside a
             transaction (rules out the existing migration runner without a change).
             Option B — take a 6-minute maintenance window during the 03:00 UTC low-traffic
             slot, keep the existing runner unchanged.
             Recommendation: B — smaller blast radius, no migration-runner change, and the
             window is well inside the existing maintenance SLA.",
  urgency:  "med"
})
```

- `subject`: one line, the decision — this is the issue title and the dashboard row.
- `context` (required): what you are doing, what you found, why you are stuck. **The
  reader has not seen your transcript** — never write "the list above," "those items," or
  anything that assumes shared context. State it fresh.
- `question` (required): current state → desired state → proposed change, then options
  with tradeoffs, then your own recommendation. Don't just describe the fork — say which
  branch you'd take and why.
- `ask` (optional): a list of `{ question, header?, options: [{ label, description? }] }`
  when the decision is a discrete set of choices. These render as buttons on the dashboard
  — use this instead of asking the human to type a free-text answer when the answer really
  is "pick one of N."
- `urgency` (optional, default `med`):
  - `low` — whenever it's convenient, no deadline pressure.
  - `med` — needed today.
  - `high` — blocking something that will stall soon if unanswered.
  - `blocking` — you cannot proceed at all right now.
- `repo` (optional): only pass this to target a **different** repo than your current
  working directory's GitHub remote. The shim fills `repo` from cwd automatically —
  omit it in the common case.
- `parent` (optional): only set this when a real issue already exists to attach to —
  `<n>` for an issue in the resolved repo, or `owner/name#<n>` to point at a specific
  issue in another repo. For Legion roles, `parent` is the root issue you're working
  under. Omitting it creates a standalone top-level thread; don't invent a parent that
  doesn't exist.

## After dispatching

The reply arrives back in your own session as a steer — you do not fetch it. Keep every
other non-blocked lane of work moving while you wait; a dispatch is not a reason to go
idle. Never poll GitHub for the reply and never set up a watcher, cron, or retry loop to
check on it — the delivery mechanism is already push-based. The issue URL returned by the
tool (`{"thread": N, "url": "..."}`) is the durable record; you don't need to keep it
anywhere else.

If the tool errors because it can't resolve a repo (cwd has no GitHub remote and no
qualified `parent`), pass `repo=owner/name` explicitly and retry.

**Named risk:** a thread created in a repo where the Envoy GitHub App is not installed
gets created successfully, but replies to it never route back to your session — you'll
wait forever for a steer that can't arrive. Coverage is every repo under `sjawhar`, plus
`acme-org/example-repo` and `acme-org/example-e2e` — the App is
deliberately not installed org-wide. That list can change; to check a repo before relying
on a reply, `gh api /repos/<owner>/<name>/installation` returns 200 when the App is
installed there and 404 when it is not.
