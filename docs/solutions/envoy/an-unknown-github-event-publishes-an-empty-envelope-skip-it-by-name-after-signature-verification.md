---
title: "An unknown GitHub event publishes an empty envelope to the repository's `comment` topic: prove it with a publish count, not a status code, and skip it by name after signature verification"
category: envoy
tags:
  - webhook
  - github
  - merge_group
  - normalize
  - githubSkip
  - tdd
  - listener-rollout
date: 2026-09-14
status: active
module: envoy
related_issues:
  - "LEGION-99"
  - "sjawhar/legion#1089"
  - "LEGION-114"
symptoms:
  - "a session subscribed to notifications.github.<owner>.<repo>.> receives a delivery whose summary is `<event> <action>` (e.g. `merge_group checks_requested`) with an empty payload"
  - "the daemon's durable consumer logs a consumed GitHub event whose payload fails JSON.parse and reduces to nothing"
  - "a spec says the webhook must 'answer 200 and publish nothing' for an event and the handler already answers 200"
---

# An unknown GitHub event publishes an empty envelope to the repository's `comment` topic

Envoy's GitHub webhook (`packages/envoy/internal/webhook/github.go`) does not refuse an event type
it has never heard of; it normalizes it through defaults that each look harmless and together
publish one contentless envelope. LEGION-99 met this with GitHub's `merge_group` event and fixed it
for that event by name; the default itself is LEGION-114.

## Where the empty envelope comes from

In `packages/envoy/internal/contracts/normalize.go`, for an event with no case of its own:

- `githubKind` falls through to `"comment"`;
- `githubNumber` returns `""`, so `githubTopic` falls through to
  `GithubSubject(owner, repo, "comment")` — the **repository-wide** `notifications.github.<owner>.<repo>.comment`;
- `githubSummary` falls through to `fmt.Sprintf("%s %s", event, action)` — non-empty, so
  `Validate()`'s `payload_summary` check passes;
- `githubPayload` falls through to `""`, and `Payload` is not a required field of the generated
  envelope schema, so `Validate()` passes;
- `GithubEnvelopes` returns that one envelope, and the handler publishes it and answers 200.

Every consumer of the repository-wide topic then sees a delivery with summary `merge_group
checks_requested` and nothing else. The Legion daemon's durable consumer (`notifications.github.
<owner>.<repo>.>`) receives it, `payloadFrom`/`recordPayload` swallow the `JSON.parse("")` error,
and the reducer returns no effects — one no-op durable message per event, two per queued pull
request (`checks_requested`, `destroyed`). Harmless to the daemon; noise to any session subscribed
repo-wide; and wrong for a spec that says "publishes nothing".

## Prove "publishes nothing" with a publish count, never with the status code

LEGION-99's spec first read "changed only if the current handler errors on the event". The handler
does not error — it answers 200 — and a check written as "does it 200?" would have passed and
shipped the junk envelope. The planner found the defect by running the handler with a documented
`merge_group` payload and reading the publisher: `status=200, published=1,
topic=notifications.github.sjawhar.legion.comment, payload=''`. The assertion the requirement
needs is `len(pub.published) == 0` **and** `len(recorder.calls)+len(recorder.suiteCalls)+
len(recorder.headCalls) == 0`, with the status as a third check, not the only one.

The test that carries it (`TestGitHubHandlerMergeGroup` in `github_test.go`) was written first and
run red before the handler changed — `published = 1, want 0; first topic
"notifications.github.sjawhar.legion.comment"` on both signed subtests — and that red output went
into the implement handoff and the PR body as the proof the change was needed. The tester then
reproduced it independently by reverting `githubSkip` to `return false` uncommitted, watching the
same failure reappear verbatim, and `jj restore`-ing. Hold future test phases to that: break the
fix and see the reported symptom, not just re-run the green suite.

No recorded `merge_group` delivery existed when the test was written (the queue was not on yet),
so the fixture (`testdata/merge_group_checks_requested.json`) is production-shaped from GitHub's
documented payload and the test comment says so — the same convention as `sub_issues_added.json`.
Replace it with a captured delivery once one exists, but do not wait for one to write the test.

## The skip runs after signature verification, and one subtest pins that

The handler's order is method → body (1 MiB cap) → `X-GitHub-Delivery`/`X-GitHub-Event` →
`verify.Github(secret, body, signature)` (401) → JSON decode → sender log → `githubSkip(event)`
(200 `ok`, return) → head recording → CI observations → envelopes → publish. `githubSkip` was the
designed hook for "acknowledge and drop" and already sat after verification; the change is one
comparison:

```go
func githubSkip(event string) bool {
	return event == "merge_group"
}
```

A skip that ran *before* verification would let anyone POST an unsigned `merge_group` and get 200.
Nothing in the code enforces the order — `githubSkip`'s comment describes its call site, and a
comment about the caller goes stale silently if `GitHubHandler` is reordered — so the test's third
subtest is the guard: the same fixture with `X-Hub-Signature-256: sha256=invalid` must answer 401
with nothing published. Any new early return added to a webhook handler that verifies signatures
gets the same unsigned subtest.

## Skip by name is the scoped fix; the default is the defect

LEGION-99 skipped exactly `merge_group` because its spec scoped the Envoy change to that event. The
next unknown event GitHub adds will publish the same empty envelope until LEGION-114 makes the
default safe (an unknown event type should be acknowledged and dropped, or refused into a
dead-letter, never routed to `comment`). A worker asked to add a second name to `githubSkip` should
fix the default instead and turn the names into the test table; a worker touching
`normalize.go`'s defaults should keep `TestGitHubHandlerMergeGroup`'s three subtests green as the
regression lock. The reviewer's fast-follow for #1089 (fold the merge-group test into
`TestGitHubHandler`'s table; drop the call-site sentence from the comment) rides LEGION-114.

## The fixed listener is not autodeployed

A merge that touches `packages/envoy/**` runs `Release Envoy Listener`
(`.github/workflows/release-envoy-listener.yaml`, `on: push: branches: [main]` with that path
filter) and publishes `ghcr.io/sjawhar/legion/envoy:<merge sha>` and `:latest`. But
`packages/envoy/deploy/scripts/autodeploy.sh` runs `up -d --no-deps dispatch` only (lines 108 and
145): the **listener** container keeps the old image until the operator restarts it on the new tag.
Until then the pre-fix behaviour continues in production — one `merge_group checks_requested` and
one `merge_group destroyed` junk delivery per queued pull request on the repository's `comment`
topic. The implementer's post-merge production check therefore (a) names the image tag to the
operator session — only the operator touches the box — and (b) subscribes to
`notifications.github.<owner>.<repo>.comment` through one queue cycle and reports which build the
listener runs by whether the junk deliveries still arrive. If GitHub is not delivering
`merge_group` to the App at all (event subscription), neither appears in either case; say so
rather than reading silence as the fix.
