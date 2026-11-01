---
title: "A new OK-line mark anchors to the line's end, not to its neighbours, and the consumer requires it by name with the remedy"
category: daemon
tags:
  - bootprobe
  - probe-image
  - ok-line
  - probe-marker
  - regexp
  - worker-image
date: 2026-10-08
status: active
module: packages/daemon/internal/bootprobe, packages/daemon/internal/runtime/sandbox
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1846"
  - "LEGION-629"
---

# A new OK-line mark anchors to the line's end, not to its neighbours

Extends docs/solutions/daemon/the-consumer-side-of-a-probe-marker-require-it-only-under-the-setting-that-needs-it-and-cache-the-confirmation-not-the-pass.md.

- The Go daemon reads `legion probe-image`'s OK line with one pattern per mark, each built by
  `bootprobe.markPattern`: the prefix, `.*`, the mark (with a captured value when it ends in `=`),
  then `markThenContract` — `(?: .*)? daemon-api-version=[0-9]+$`. A mark is anchored to the
  line's start and its contract token at the end, never to the mark beside it, so a line from an
  older CLI carrying fewer marks and from a newer CLI carrying more both parse.
- Add a mark by declaring it through `markPattern`, placing it anywhere before the contract token,
  and reading it with `markValue`. A hand-rolled regex that names a neighbouring mark stops
  matching the moment another tree adds its own mark between them, and the daemon then refuses
  every image at boot for a reason the message cannot name.
- The consumer (`sandbox.judge`) requires a new mark by name, after the contract check, with the
  remedy in the refusal: `its legion CLI predates the check: build the image from this daemon's
  commit`. A mark the probe prints but no consumer requires certifies nothing.

## Evidence

sjawhar/legion#1846 added `capabilities=checked` and `model-fallback=on|off` to the OK line between
`agent-models=<state>` and `daemon-api-version=<N>`. The existing `agentModelsState` pattern was
`agent-models=(\S+) daemon-api-version=[0-9]+$`, so the new marks would have silently stopped it
matching and every probe pod would have been refused "without resolving the prompt-named agents'
models". The shared builder replaced the three literal patterns; `bootprobe_test.go` parses the
new line and the old one (no new marks) through each reader. LEGION-629 (#1848) adds
`extensions=discovered` to the same line through the same builder.
