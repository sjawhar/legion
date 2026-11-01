---
title: "A query's error branch is asserted after the query reaches `error`, not after the mock is called; a per-item field added to a list response is gated on `!== undefined`"
category: testing
tags:
  - tanstack-query
  - react-query
  - testing-library
  - waitFor
  - flaky-test
  - false-positive
  - api-versioning
  - last_reply
date: 2026-09-14
status: active
module: packages/dispatch/web
related_issues:
  - "LEGION-62"
  - "sjawhar/legion#1088"
symptoms:
  - "a unit test written to lock a fix passes when the fix is reverted"
  - "`waitFor(() => expect(mock).toHaveBeenCalledWith(...))` followed by assertions about the failure rendering"
  - "`waitFor(() => expect(screen.queryByText(...)).toBeNull())` used to wait for a degraded state"
  - "an indicator computed from a new response field is wrong against an older server that does not send it"
---

# Assert a Query's Error Branch After the Query Reaches `error`; Gate a New Per-Item Field on `!== undefined`

Two client-side facts from #1088 (LEGION-62), both caught by the reviewer reading the test rather
than running it.

## The mock being called is not the query having failed

`GitHubLink` runs two TanStack queries: the pull request reference (title, state) and, once that
resolves, the check-runs. The fix under test was "a failed check-runs read drops only the checks
pill and keeps the loaded title and state". The first version of the lock did this:

```ts
await screen.findByText(title);
await waitFor(() => expect(githubRest).toHaveBeenCalledWith(".../check-runs"));
await waitFor(() => expect(screen.queryByText(/^checks:/)).toBeNull());
expect(screen.getByText(title)).toBeDefined();   // asserted while checks may still be pending
```

Every wait is satisfied before the failure has happened. `toHaveBeenCalledWith` passes the instant
the query function is invoked, before its rejection is processed. `queryByText(...).toBeNull()`
passes on the first tick because the pill renders nothing while loading *and* nothing on error.
TanStack delivers the error through `notifyManager` (a `setTimeout 0`), so whether `isError` has
committed by the title assertion depends on act's flush order — and while the query is pending,
the pre-fix code (`failed = reference.isError || checks.isError`) showed the title too. The test
was green on the code it was meant to catch.

The deterministic form waits on the query state itself, through the `QueryClient` the component
renders with, then asserts synchronously and scoped to the element under test:

```ts
const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
const view = renderIssuePage("/issues/CORE-1", undefined, undefined, queryClient);
await waitFor(() =>
  expect(queryClient.getQueryState(["github-link-checks", url, sha])?.status).toBe("error")
);
const link = screen.getByRole("link", { name: /#7 / });
expect(within(link).getByText(title)).toBeDefined();
expect(within(link).getByText("open")).toBeDefined();
expect(within(link).queryByText(/^checks:/)).toBeNull();
```

Checked the way every lock should be: 5/5 red with the pre-fix `GitHubLink.tsx` swapped in,
green with the fix. The rule: a test of "what renders when the query fails" waits for
`getQueryState(key).status === "error"` (or `"success"` for the other branch), never for a
mock call, never for the absence of an element that is also absent while loading.
`renderIssuePage` in `IssuePage.test.tsx` already takes the `QueryClient` as its fourth argument
for exactly this.

## A field added to an existing list response: `undefined` means old server, `null` means no data

`GET /api/v1/issues/{key}` gained `last_reply` on every `open_asks` row so the header's whose-turn
badge can apply the Inbox's rule (`waitingOnYou`: an open ask whose newest reply is not a human's
waits on the human). The field is `null` when nobody has replied and `{author, created_at}`
otherwise — and it is simply absent from a server that predates the change. The spec's error row
says: without the information the indicator is not shown rather than shown wrong. So the client
distinguishes the two:

```ts
const knowsWhoseTurn = openAsks.every((ask) => ask.last_reply !== undefined);
const whoseTurn = openAsks.length === 0 || !knowsWhoseTurn ? null : ...;
```

A truthiness check (`ask.last_reply ? ... : ...`) would have treated "old server" and "no reply
yet" identically and shown `Waiting on you` against a server that never said so. When a client
adds a reading of a new per-item field, check `!== undefined` per item; `null` is data. The Go
side writes the key explicitly (`json:"last_reply"` without `omitempty`) so a genuine "no reply"
is a `null` on the wire, never an absent key — the Go test pins that the key is present and null
before any reply, then a `user` author after a human reply, then a `session` author after the
agent's.

## Related

- `docs/solutions/legion/a-layout-fix-is-tested-at-the-band-where-the-old-and-new-rule-disagree.md`
  — the review rounds this came from.
- `docs/solutions/testing/race-regression-tests-that-fail-before-the-fix.md` — the general rule
  that a lock is proven red on the pre-fix code.
