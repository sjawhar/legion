---
title: "A finding outside your issue is an ask on the report issue, never an issue you file"
category: legion
tags:
  - process
  - findings
  - no-deferrals
  - controller
  - asks
date: 2026-10-24
status: active
module: skills/legion-worker
supersedes:
  - docs/solutions/legion/a-fast-follow-line-that-promises-a-pull-request-after-the-merge-has-no-owner-once-the-tree-is-signed-off-land-the-cleanup-in-the-running-review-round-or-write-none-LEGION-695.md
  - docs/solutions/legion/out-of-spec-findings-go-to-the-architect-and-come-back-as-spec-versions.md
  - docs/solutions/legion/a-child-of-two-open-pull-requests-waits-for-main-re-verifies-every-cited-symbol-and-treats-the-pane-as-a-surface-to-check.md
  - docs/solutions/testing/a-flake-spec-names-no-cause-before-the-retained-trace-is-read.md
  - docs/solutions/legion/a-fix-that-produces-the-next-finding-is-a-ratchet-stop-it-with-a-rule-written-early-INFRA-66.md
related_issues:
  - "sjawhar/legion#2130"
---

# A finding outside your issue is an ask on the report issue, never an issue you file

The five learnings above tell an agent to file a problem it found outside its own work as an issue:
the cleanup, the out-of-scope finding, the library bug, the follow-up. Where they say that, they
are wrong now. No Legion role, and no subagent of one, files an issue for a finding outside its own
issue: it opens a `dispatch ask` on the controller's report issue, naming the issue it came from,
where the fix belongs and a recommendation, and a person decides whether it needs an issue; the
controller files one only on that person's answer (`skill://legion-worker/references/findings.md`).
A problem inside the tree still goes to the tree's architect, which may file it as a child of the
tree. The rest of each learning stands.

## Evidence

Sami ruled on 2026-10-23 that an agent or the controller opens an ask and he decides whether a
finding needs a new issue. Over the three days before, Legion agents had filed 78 of a deployment's
project's 158 issues, and the controller had admitted 21 of its own filings to `todo` itself.
