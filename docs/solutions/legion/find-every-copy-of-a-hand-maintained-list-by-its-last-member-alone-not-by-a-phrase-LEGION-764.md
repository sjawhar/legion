---
title: "Find every copy of a hand-maintained list by its last member alone, not by a phrase"
category: legion
tags:
  - grep
  - duplication
  - docs
  - dockerfile
  - capabilities
  - simplify
date: 2026-10-15
status: active
module: packages/daemon/internal/capabilities
related_issues:
  - "LEGION-764"
  - "sjawhar/legion#1967"
---

# Find every copy of a hand-maintained list by its last member alone, not by a phrase

Extends docs/solutions/legion/text-only-skill-pr-mechanics.md (§5: two copies of one rule, edit both, grep
both). The capability check's toolchain names are one list written out in Go (`image.go`'s `onPath` call, the
row's `Summary` in `capabilities.go`), in two test stub lists and one expected string, three times in the
Dockerfile's comments and once in its toolchain step, and more than once in `docs/kubernetes.md`. Prose copies
differ in markup — `uv and kubectl`, `uv, kubectl`, `` `uv` and `kubectl` `` — so a grep for the phrase you
remember misses the one whose words are wrapped in backticks or split by a line break.

- To enumerate the copies, grep for the list's last member alone (`grep -rn 'kubectl' --include='*.go'
  --include='*.md' --include='Dockerfile' .`) or for a markup-tolerant pattern (`uv.{0,3} and .{0,3}kubectl`),
  and triage every hit; never for the phrase as one file spells it.
- Run the grep twice: when planning, to list the sites, and after implementing, when the new member is in
  every copy you changed — a copy still ending at the old last member is the one you missed. Do the second grep
  before the simplify pass, not instead of it.
- Prefer one copy with a test over many: the `Summary` string no test checks and no surface renders is the copy
  that drifts first (the reviewer's fast-follow on #1967).

## Evidence

The LEGION-764 plan enumerated fifteen sites and seven deliberately unchanged, and still missed
`docs/kubernetes.md:157`, the probe subsection's own sentence, which reads `` `go`, `curl`, `wget`, `python3`,
`node`, `bun`, `uv` and `kubectl` on `PATH` `` with backticks on every name. The implementer's post-change grep
`uv and kubectl|uv, kubectl` matched only `worker.Dockerfile` and `capabilities.go` and so confirmed nothing
about that line. The `ce-simplify-code` reuse reviewer found it by reading the file; the fix went into the
simplify head 40b71edb (sjawhar/legion#1967) and cost a second `Worker Image` build. The same run also found a
comment in `internal/runtime/sandbox/live_lifecycle_test.go` stating `the worker image has no jq` — true until that
push, false after it — which no list grep would reach: grep for the claim (`no jq`) as well as for the list.
