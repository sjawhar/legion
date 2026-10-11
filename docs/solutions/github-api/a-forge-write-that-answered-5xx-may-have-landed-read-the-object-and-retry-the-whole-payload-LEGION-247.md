---
title: "A forge write that answered 5xx may have landed: read the object it targets, and retry only the whole payload"
category: github-api
tags:
  - github-outage
  - http-500
  - pull-request-body
  - push
  - idempotence
  - verification
date: 2026-10-07
status: superseded by LEGION-631 — `legion gh` and `legion push` are gone; the same reads and retries are plain `gh` as the role's App and plain `jj git push --bookmark legion/<KEY>`, and the read-the-object-before-retrying rule stands
module: legion gh, legion push
applies_when:
  - A `legion gh -- api --method PATCH`, a `gh pr edit` or a GraphQL mutation answers `HTTP 500`, `Something went wrong`, or an empty body
  - `legion push` reports `remote: Internal Server Error` or `The remote rejected the following updates`
  - githubstatus.com shows Git Operations, Pull Requests, Webhooks or Actions degraded
related_issues:
  - "LEGION-247"
  - "sjawhar/legion#1831"
---

# A forge write that answered 5xx may have landed

- A `5xx`, a GraphQL `Something went wrong`, or `remote: Internal Server Error` on a write is
  **unknown**, never **refused**. Before retrying, and before reporting the write as not done,
  read the object the write targets: `api repos/{o}/{r}/git/ref/heads/legion/<KEY>` for a push
  (not `pr view`, whose head lags the ref under a Pull Requests or Webhooks outage), the live
  body for a body `PATCH`, the comments list for a comment.
- Retry with the whole intended payload only. Never probe a failing write path with a stub on an
  artifact other roles read (`{"body":"probe"}`, a one-field `PATCH`): the probe lands the
  moment service recovers and is what every reader sees until the real write succeeds. A no-op
  probe is the live content written back to itself, nothing else.
- Check githubstatus.com's summary before the second retry; an outage there turns the retry loop
  into a wait with backoff, and the request id in the response is what a report to the architect
  carries.
- Record in the handoff which writes are confirmed, which are pending, the window and the request
  id, so the next phase can tell a forge write that is pending from one never attempted.

## Evidence

sjawhar/legion#1831, 2026-10-07 ~15:07–15:30Z, GitHub's partial outage (Git Operations, Pull
Requests, Webhooks and Actions `major_outage` on githubstatus.com): every `PATCH` of the pull
request's body answered `HTTP 500` with an empty body — the intended body, the live body written
back to itself, and a trivial field alike — and `gh pr edit` answered GraphQL `Something went
wrong` (request id `5D06:64BC8:D8ED3:12B66C:6AC660AE`). A probe `{"body":"probe"}` sent to see
whether any write got through succeeded a few minutes later and replaced the body with one word
until the full body was written back seconds after. `legion push` of the implement handoff commit
84d0a35c answered `remote: Internal Server Error` (request `EFE9:19CEFD:2C9264:345E41:6AC660FB`)
and `Failed to push some bookmarks` four times, yet `git/ref/heads/legion/LEGION-247` already
read 84d0a35c, and the next `legion push` said `already matches`; `pr view` kept answering the
previous head 013b1415 until the tester's push synchronised the pull request. The implement
handoff's `openQuestions` recorded the pending body write with the request id, which is how the
tester read the stale CI line as GitHub's lag and not the implementer's omission.
