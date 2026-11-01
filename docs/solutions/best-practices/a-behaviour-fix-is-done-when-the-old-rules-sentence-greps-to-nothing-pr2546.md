---
title: "A behaviour fix is done when the old rule's sentence greps to nothing"
category: best-practices
tags:
  - census
  - documentation
  - published-rules
  - review-rounds
date: 2026-10-31
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "sjawhar/legion#2546"
---

# A behaviour fix is done when the old rule's sentence greps to nothing

Extends docs/solutions/legion/a-change-that-retires-a-path-or-mechanism-greps-the-old-string-repo-wide-not-the-diff-LEGION-636.md.

That note greps a retired *path or mechanism*. A behaviour that stays but changes its rule is the
same census with no deletion to anchor it: nothing disappears, so a diff review sees only the
handler.

- After changing a behavioural rule, pick the old rule's most distinctive sentence — the clause a
  writer would copy, not a symbol name — and grep it across the whole repository. Fix every copy
  in the same commit as the behaviour.
- Expect the copies in surfaces no symbol reaches from the code you changed, and read each of
  these before you call the sweep finished:
  - the route index or command table served to clients;
  - the field documentation of the request and response types;
  - the migration header that explains why the column exists;
  - the agent-facing guides and route reference for the package;
  - comments on the tests that pin the behaviour, and on tests in other packages;
  - acceptance fixtures and shell drivers, whose comments name the check they run;
  - the changelog fragment, which is written once and never re-read;
  - any refusal string a test pins by its text, where the sentence is user-visible output.
- A rule whose wording you have just rewritten is the next sweep's search term. Record the grep
  and its remaining hits in the round's report, so the reviewer re-runs it instead of reading for
  it.

## Evidence

sjawhar/legion#2546 corrected which item a reply's named child is resolved against. The handler
change was `packages/envoy/internal/dispatch/api/comment_create.go` in `dd2f17197c3f`, but the old
sentence — "a reply in an ask's thread" — survived in nine more places, and the second review
round was spent on them. `1b769383daf4` swept it:

- `packages/envoy/internal/dispatch/api/routes_table.go:142`, the route index served to clients;
- `packages/contracts/src/dispatch-api.ts`, three separate doc comments (the reply field, the
  reply-block type and the create-comment route);
- `packages/envoy/cmd/dispatch/AGENTS.md:646`, the route reference an agent reads;
- `packages/envoy/.changes/unreleased/pr2546-ask-parts.yaml`, the changelog fragment;
- `packages/envoy-client/src/dispatch-execute.ts`, where the sentence was a refusal string
  (`"a reply in an ask's thread answers a part of that ask"`) that
  `src/__tests__/dispatch-execute.test.ts` pins;
- `packages/envoy/internal/dispatch/store/migrations/20261101143211_ask_blocks.up.sql`, the
  migration header explaining the split reply arm;
- `packages/envoy/internal/dispatch/store/graph_edges_test.go:182-185` and
  `packages/envoy/internal/dispatch/api/ask_parts_test.go`, test comments;
- `packages/dispatch/e2e/acceptance/block-graph.sh`, three lines: the driver's header, the
  section comment and the `R11` check's own comment.

None of those files is reachable from `comment_create.go` by any symbol.
