---
title: "A change to shown text or a title runs every Dispatch e2e spec that asserts the old value"
category: testing
tags:
  - dispatch
  - e2e
  - playwright
  - expect-soft
  - issue-pod
date: 2026-10-26
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-440"
  - "sjawhar/legion#2237"
---

# A change to shown text or a title runs every Dispatch e2e spec that asserts the old value

Extends docs/solutions/legion/proof-without-the-rig-what-the-daemon-suite-and-ci-caught-and-the-one-gap-they-left.md.

That document says to grep the CI-only suites for a changed counter or log line. The Dispatch
browser suite needs the same step, because an issue pod with two CPUs cannot run the whole suite
in useful time and CI's `dispatch` job runs all of it:

- After you change visible text, a tooltip (`title`), an accessible name or another attribute a
  locator can read, run `rg` over `packages/dispatch/e2e` for the old value and for the element's
  locator, and read each hit. Run every spec that matches, on each project that runs it. Running
  the specs you edited and the acceptance spec is not enough.
- Look in viewport-named specs first. `phone.e2e.ts` and `layout.ts` assert many components'
  text and titles, and a file named after a viewport is never picked by "the specs for the files I
  changed".
- An `expect.soft` failure reports nothing until its spec runs. Treat a soft assertion on the old
  value as a red test you have not seen yet.
- When the whole suite cannot run in the pod, write down in the proof which specs ran at which
  head. A spec that timed out in a long run is re-run alone at the same head before it counts as a
  failure; in a 2-CPU pod, long two-browser or drag specs hit the 30 s test timeout under load and
  pass alone.

## Evidence

On sjawhar/legion#2237 the Inbox asker's tooltip changed from `<label>` to
`<label> · <full id>`. The implementer ran `agent-name`, `inbox` and `claim`, and the tester ran
`agent-name` and `claim`. Review round 3 found `phone.e2e.ts:849`, which runs on chromium and
iphone with no skip: `expect.soft(authorChip.getByText(longTitle)).toHaveAttribute("title",
longTitle)` now fails. That cost one more review round and one more tester round for a one-line
fix. With the old assertion the spec fails on the new build (received the title plus
` · e2e-long-title`); with the new one it passes on both projects.

Earlier on the same pull request, a whole-suite run in the pod took over an hour and was cut off
by a process restart. Its only failures were timeouts in board, inbox-stability, margin and
editor-paste specs that this pull request did not change. Each passed when run alone at the same
head.
