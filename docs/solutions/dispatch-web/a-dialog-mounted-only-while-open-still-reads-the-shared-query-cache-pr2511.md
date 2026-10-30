---
title: "A dialog mounted only while open still reads the shared query cache"
category: dispatch-web
tags:
  - tanstack-query
  - stale-time
  - dialog
  - advice-reads
date: 2026-10-29
status: active
module: packages/dispatch/web/src/features
---

# A dialog mounted only while open still reads the shared query cache

A dialog that is mounted only while it is open starts its *own state* fresh each time. Its reads
do not: the `QueryClient` lives in `web/src/main.tsx`, above the router, so a query the dialog ran
on the last open is answered from the cache on the next one — within `staleTime: 30_000` with no
request at all, and up to the default five-minute `gcTime` with the old body shown at once while a
refetch runs behind it.

- **A read whose answer is advice about what exists right now, with no event that invalidates its
  key, sets `staleTime: 0` and `gcTime: 0`.** `staleTime` alone still paints the stale body first,
  and a "still checking" flag computed from `data === undefined` stays false through it, so the
  surface claims to be current while showing the old answer. Write the reason beside the options:
  the next reader has to know why this query opts out of the app's defaults.
- **Check first whether an event can carry the invalidation.** The package's event rules
  (`api/event-refresh.ts`) are the normal answer; opting out of the cache is for a key no write
  and no feed knows about, such as a check run over text a person is typing.
- **Prove it with a row that writes through the API between two opens of the same dialog on the
  same input.** Nothing else distinguishes a fresh read from a cached one, because both render the
  same component with the same props.
- The server-side analogue argues the other way
  (`docs/solutions/testing/a-cache-under-a-read-the-browser-suite-drives-needs-an-off-switch-not-ttl-zero-pr2429.md`:
  an off switch, not a TTL of zero). The cases differ: that cache coalesces concurrent readers,
  while two opens of one dialog share no in-flight fetch to join.

## Evidence

sjawhar/legion#2511. The create dialog's related-item check inherited the app's `staleTime:
30_000` and the default `gcTime`, and nothing invalidates its key: the create's `onSuccess`
invalidates the issue list and the projects query only. A person could file an issue, press `c`,
type that title again and be told the project held nothing like it.

`CreateIssueDialog.tsx`'s own doc comment is what made the cache invisible — "Mounted only while
open, so its draft and error state start fresh each time" is true of local state and of nothing
else. Found in review, fixed by giving the query `staleTime: 0` and `gcTime: 0`.

The row is `e2e/create-issue-related.e2e.ts`, "a title checked before an issue was filed is
checked again, not answered from before": type a title, see the found-nothing form, press Escape,
file an issue with that title through the API, reopen with `c`, type the same title, expect its
link. With both options removed it is red 3 of 3 at the link's `toBeVisible`, `element(s) not
found`. The tester measured the cost the options were suspected of: one check at the first rest,
one after a pause, one after a refocus — the fresh read costs a request per settled title, which
the 400 ms debounce already bounds, not a refetch loop.
