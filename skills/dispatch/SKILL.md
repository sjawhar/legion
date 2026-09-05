---
name: dispatch
description: Use when you need a human decision from Sami or the user — asking a question, waiting on approval, saying "let me know", writing an end-of-message question block, picking between options with tradeoffs, or starting unattended/long-running work that will need input later. Raises the question as a durable GitHub-issue thread instead of a transcript question nobody will see.
---

# Dispatch

`dispatch` turns a question into a durable, dashboard-visible GitHub-issue thread instead
of a line buried in a transcript. A thread is **one decision, held as a conversation**: it
opens with your question, the human replies, and if the reply changes the question you ask
again on the same thread. It closes when the decision is settled, not when the first reply
lands. Use it any time you would otherwise leave a question for a human to notice on their
own.

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

## Writing for the reader

The reader has not seen your transcript. They are looking at a card on a dashboard between
other things, and they will answer the question you wrote, not the one you meant.

- **No nouns you coined this session.** No "the list above", "those items", "lane B",
  "the eval-set thing". No internal identifiers — eval-set ids, lane names, hashes,
  session ids — unless the question is about them. Expand every identifier on first use.
  GitHub references may be bare: `#482`, `acme-org/example-repo#17`, or a URL — the
  dashboard unfurls them into their titles.
- **Structure over paragraphs.** `context` is at most three short paragraphs or a bullet
  list, one idea each: what you are doing, what you found, why you are stuck. `question`
  is a list: current state → desired state → options with one-line tradeoffs → your
  recommendation. Don't just describe the fork — say which branch you'd take and why.
- **Options are buttons.** If you are offering choices, put them in `ask`; never enumerate
  them in prose. A human answers a button in one click and the answer arrives structured;
  a choice buried in a paragraph arrives as a sentence you have to interpret.
- **Length caps.** `context` is at most 1200 characters, `question` at most 800. The
  service refuses a longer call before anything reaches GitHub, naming the field and the
  limit (`context is 1450 characters; the limit is 1200`); nothing is truncated. When you
  hit a cap: move the choices into `ask`, cut background the reader does not need to
  decide, and if there really are two decisions, open two threads.

### Before / after

A real thread, as first written — one block, coined nouns, options in prose:

```
subject:  "e2e submitter"
context:  "So I've been going through the e2e harness like we discussed and the submitter
           identity thing from earlier is still broken because the fixture in lane B uses
           the shared bot account which means the assertions in eval-set 7f3a can't tell
           who submitted, and the thing I mentioned about the env var not being set in CI
           is also true so basically the list above doesn't apply until we pick one, and I
           looked at how the other repo does it and they hardcode it which is fine for them
           but not us, also the token minting issue is related but separate."
question: "should I add E2E_SUBMITTER_EMAIL or use the bot or something else? I think
           probably the env var but not sure, or maybe the hardcode thing, or the third
           option where the harness reads it from git config. Let me know."
```

The same decision, written for the reader:

```
subject:  "E2E harness: which identity submits test PRs?"
context:  "- The end-to-end test harness (acme-org/example-repo#17158) submits PRs as the
             shared bot account, so a run cannot tell test submissions apart from real ones.
           - CI sets no submitter identity today; the harness has no setting for one."
question: "- Current: every test PR is authored by the shared bot.
           - Desired: each run's PRs carry an identity that marks them as test submissions.
           - Recommendation: a dedicated identity from one CI environment variable —
             smallest change, no per-machine setup."
ask:      [{ header: "Test submitter identity",
             question: "Which identity should the harness use?",
             options: [
               { label: "Env var E2E_SUBMITTER_EMAIL", description: "one CI variable; the harness reads it" },
               { label: "Shared bot (status quo)",      description: "no change; runs stay indistinguishable" },
               { label: "Read from git config",         description: "per-machine setup; drifts" } ] }]
```

## How

### Opening a thread

```
dispatch({
  subject:  "Postgres migration: run online or take a maintenance window?",
  context:  "- Implementing the users-table index migration for #482.
             - The table has 40M rows; a plain CREATE INDEX locks writes for ~6 minutes
               in the staging timing test I just ran.",
  question: "- Current: no online-migration tooling in this repo's deploy pipeline.
             - Desired: index added without a customer-visible write outage.
             - Recommendation: the maintenance window — smaller blast radius, no
               migration-runner change, inside the existing SLA.",
  ask:      [{ header: "Approach", question: "Which way?",
               options: [
                 { label: "CREATE INDEX CONCURRENTLY", description: "no lock, ~25 min, needs a migration-runner change" },
                 { label: "6-minute window at 03:00 UTC", description: "runner unchanged, brief write outage" } ] }],
  urgency:  "med"
})
```

- `subject`: one line, the decision — this is the issue title and the dashboard row.
- `context` (required, ≤ 1200 chars): what you are doing, what you found, why you are stuck.
- `question` (required, ≤ 800 chars): current → desired → options → recommendation.
- `ask` (optional): a list of `{ question, header?, options: [{ label, description? }],
  multiple?, custom? }`. These render as buttons; the human can always type a free answer too.
- `urgency` (optional, default `med`): `low` — whenever convenient; `med` — needed today;
  `high` — something will stall soon; `blocking` — you cannot proceed at all right now.
- `repo` (optional): only to target a **different** repo than your working directory's
  GitHub remote; the tool fills it from the cwd otherwise.
- `parent` (optional): only when a real issue exists to attach to — `<n>` in the resolved
  repo or `owner/name#<n>` elsewhere; `owner/name#<n>#<commentId>` also appends a
  breadcrumb to that comment. For Legion roles, `parent` is the root issue you're working
  under. Omitting it creates a standalone thread; don't invent a parent.

The tool returns `{"thread": N, "url": "..."}`; that URL is the durable record.

### Continuing a thread

When the reply changes the question — the human challenged the premise, ruled out your
recommendation, or asked for a variant you had not offered — re-ask **on the same thread**:

```
dispatch({
  thread:   "482",                      // or "owner/name#482"
  context:  "- You ruled out the maintenance window: the 03:00 UTC slot is now used by
               the nightly export.
             - CONCURRENTLY needs the migration runner to run outside a transaction.",
  question: "- Current: the runner wraps every migration in a transaction.
             - Desired: this one index built online.
             - Recommendation: a one-off flag on the runner for non-transactional
               migrations, used only by this migration.",
  ask:      [{ header: "Runner change", question: "Add the non-transactional flag?",
               options: [{ label: "Yes, one-off flag" }, { label: "No, hand-run the index" }] }]
})
```

- `thread` replaces `subject`; `urgency`, `repo`, and `parent` are not accepted with it.
  The thread must be open and be a dispatch thread; otherwise the tool says so
  (`#N is not a dispatch thread`, `#N is closed; open a new thread`).
- Same decision → same thread. A genuinely new decision → a new thread, even if it came
  up in the reply.
- **Read a challenge as an answer.** "Why not the bot?" is not a request for more prose;
  it is the human declining your framing. Say what you now know, then ask the narrowed
  question with new options. A follow-up carries your session's identity, so the human
  sees which conversation is asking even after a handoff.
- The tool returns `{"thread": N, "url": "...", "comment": "..."}`.

## After dispatching

The reply arrives back in your own session as a steer — you do not fetch it. Keep every
other non-blocked lane of work moving while you wait; a dispatch is not a reason to go
idle. Never poll GitHub for the reply and never set up a watcher, cron, or retry loop to
check on it — the delivery mechanism is already push-based. Each answer names the question
it answers, so a thread with two open questions delivers two steers.

If the tool errors because it can't resolve a repo (cwd has no GitHub remote and no
qualified `parent` or `thread`), pass `repo=owner/name` (opening) or
`thread=owner/name#<n>` (continuing) and retry.

**Named risk:** a thread created in a repo where the Envoy GitHub App is not installed
gets created successfully, but replies to it never route back to your session — you'll
wait forever for a steer that can't arrive. The App is installed per account and per repo,
not org-wide, and coverage changes; before relying on a reply from a repo you have not
dispatched to before, check it: `gh api /repos/<owner>/<name>/installation` returns 200 when
the App is installed there and 404 when it is not.

## Manual fallback: the marker format

If the tool itself is unavailable, the dashboard still understands a hand-written turn.
Every dispatch marker is an HTML comment at the very start of the body — invisible on
GitHub — followed by a blank line and the readable text.

A follow-up question, posted with `gh issue comment <n> --body-file <file>`:

```
<!-- dispatch:ask
requestId: <16 lowercase hex chars, unique to this follow-up>
ask:
    - askId: <the same 16 chars>
      question: <the question>
      header: <short header>
      options:
        - label: <label>
          description: <one line>
-->

## Context

<context>

## Question

<question>
```

A new thread, posted with `gh issue create --label dispatch-thread --title "<subject>" --body-file <file>`:

```
<!-- dispatch:thread
requestId: <16 lowercase hex chars>
urgency: med
ask:
    - askId: <the same 16 chars>
      question: <the question>
      options:
        - label: <label>
-->

**<subject>**

## Context

<context>

## Question

<question>
```

`ask` may be omitted when there are no buttons to offer. A hand-posted turn carries no
`origin`, so the dashboard cannot say which session asked, and nothing subscribes your
session to a hand-created thread — subscribe it yourself with `envoy_subscribe` to
`notifications.github.<owner>.<name>.issue.<n>.>` or the reply never arrives. Prefer the
tool.
