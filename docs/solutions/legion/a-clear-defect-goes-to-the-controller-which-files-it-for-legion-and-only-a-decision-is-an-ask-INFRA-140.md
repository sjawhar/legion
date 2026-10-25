---
title: "A clear defect goes to the controller, which files it for Legion, and only a decision is an ask"
category: legion
tags:
  - process
  - findings
  - controller
  - asks
date: 2026-10-26
status: active
module: skills/legion-worker
supersedes:
  - docs/solutions/legion/a-finding-outside-your-issue-is-an-ask-on-the-report-issue-never-an-issue-you-file.md
related_issues:
  - "sjawhar/legion#2160"
---

# A clear defect goes to the controller, which files it for Legion, and only a decision is an ask

The learning above makes every finding outside your own issue an ask, so a person answers each one
before any issue exists. That cost the owner an approval for defects whose fix held no choice of
his. A finding outside your issue now takes one of two paths
(`skill://legion-worker/references/findings.md`):

- **A clear defect** — something is broken, the evidence shows it, the fix needs no choice a person
  must make, and the fix belongs in a repository your addressing line lists. Post the evidence on
  the controller's report issue, its first line `Clear defect from <role>`, and send the controller
  one Envoy message with the defect, the issue it came from, where the fix belongs, the fix and
  that evidence. The controller files it with `--repo <owner/name>` and `--label legion`, its spec
  ending `Filed for Legion from <role> on <issue>: <evidence>`, and replies `Filed dispatch://<KEY>`
  under the evidence comment. Its triage admits such a root.
- **A finding that needs a decision** — whether to fix it at all, which of several fixes, who owns
  it, or a change of scope, cost or risk. Open a `dispatch ask` with one recommendation, and file
  nothing until a person answers. That is the path the superseded learning describes, and it still
  stands for this half.

When you are not sure the fix is clear, it needs a decision: ask. No role other than the controller
files the issue, and a problem inside your own tree still goes to the tree's architect.

## Evidence

The owner ruled on 2026-10-25 that a clear defect goes to the controller and is filed for Legion,
which reverses the filing half of the rule his 2026-10-23 ruling set. The split follows
`STRATEGY.md`: humans decide what, agents decide how, and a clear defect's fix is "how" inside work
a person already wants. `skills/legion-controller/SKILL.md` ("Free slots") names it as the one
exception to labelling work nobody asked for, and
`skills/legion-controller/references/finding-asks.md` ("File a clear defect") holds the steps.

Without `--repo`, Dispatch records the project's default repository
(`packages/envoy/internal/dispatch/api/issue_repo.go`) and the daemon admits the issue there
(`packages/daemon/internal/admit/admit.go`), so a defect in a project's second repository would
start work in the wrong workspace.
