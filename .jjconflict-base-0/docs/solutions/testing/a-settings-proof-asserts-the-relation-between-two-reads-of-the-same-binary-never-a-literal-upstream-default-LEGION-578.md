---
title: "A settings proof asserts the relation between two reads of the same binary, never a literal upstream default"
category: testing
tags:
  - live-proof
  - stage4b
  - oh-my-pi-settings
  - defaults
  - overlays
date: 2026-10-16
status: active
module: scripts/e2e/stage4b-sandbox-tree.sh
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1866"
---

# A settings proof asserts the relation between two reads of the same binary, never a literal upstream default

- Never pin an upstream tool's default as a literal in a proof (`compaction.remoteEndpoint` is
  `""`, `dev.autoqa` is `false`). Defaults move between pins, the proof runs on the image the pin
  names, and the same binary in the same pod is the only source of the default it applies.
- Read each key twice with that binary and assert the relation: under the agent process's own
  environment (`/proc/<pid>/environ`, which carries `PI_CONFIG_FILES`, so under every overlay the
  agent sees) and with no environment but `PATH` and `HOME` (no overlay: the binary's default under
  the repository's own `.omp/config.yml`). A key nothing sets reads the same both ways; a key the
  repository sets and nothing above it touches reads the repository's value both ways; a key the
  operator's overlay sets reads the overlay's value under the agent's environment and the
  repository's bare. A leaked or missing overlay shows as agreement where the two must differ, or
  difference where they must agree — without naming a single default.
- Record every pair in the note (`key=agent/bare`), so a reader sees what the pinned build answered
  without a literal in the assertion.
- The bare read must pick up the repository's `.omp/config.yml` from the working directory, or the
  "repository's value both ways" leg proves nothing: check it once in a fixture-like directory
  before relying on it.

## Evidence

sjawhar/legion#1866 (LEGION-578): Stage 4b's `repository-tools` first asserted five literals —
`compaction.remoteEndpoint=""`, `memory.backend="off"`, `images.urls.enabled=true`,
`dev.autoqa=false`, `retry.modelFallback=true` — and the second live run at ec9f48dc failed at
`compaction.remoteEndpoint reads null … want ""` after passing everything before it; the pinned Oh
My Pi also answers `dev.autoqa` `true`, so the loop would have failed again at the next key. The fix
(2e0e4171) reads each key with `agent_setting` and `bare_setting` and asserts the relation; the
passing run's note reads `compaction.remoteEndpoint=null/null memory.backend="off"/"off"
dev.autoqa=true/true images.urls.enabled=true/true retry.modelFallback=true/false`, and its control
line names the two legs that carry the proof: `retry.modelFallback` differs (the operator's `true`
over the repository's `false`), `images.urls.enabled` does not. The coordinator running the proof
proposed the same fix independently.
