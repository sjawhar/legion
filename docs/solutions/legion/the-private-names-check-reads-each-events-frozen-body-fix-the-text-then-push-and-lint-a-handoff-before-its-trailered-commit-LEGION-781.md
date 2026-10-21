---
title: "The private-names check reads each event's frozen body: fix the text, then push, and lint a handoff before its trailered commit"
category: legion
tags:
  - public-repository
  - check-private-names
  - check-pr-text
  - pr-body
  - handoff
  - skip-checks
date: 2026-10-21
status: active
module: .github/scripts
related_issues:
  - "LEGION-781"
  - "sjawhar/legion#2059"
---

# The private-names check reads each event's frozen body: fix the text, then push, and lint a handoff before its trailered commit

Extends docs/solutions/legion/a-pod-name-copied-from-dispatch-into-a-pull-request-carries-the-deployments-project-key-and-fails-the-private-names-check-LEGION-164.md.

- `check-pr-text.sh` takes the pull request's body from the event payload that started its run,
  frozen at that event (`jq '.pull_request' "$GITHUB_EVENT_PATH"`), not from a live read. A red
  `PR Title` on a body line goes green only on a *new* event with the corrected body — the next
  push, or a `pull_request` edit — never by `gh run rerun --failed`, which replays the old payload.
  Fix the body, then push; do not spend reruns on it.
- A handoff push (only `.legion/`, `skip-checks: true`) starts no workflow, so nothing lints a
  handoff's text until the next code push, which then fails `lint` on it. Run
  `.github/scripts/check-private-names.sh .legion/<issue>/<phase>.json` over the file before its
  commit, every role: a pod label copied from the cluster spells the deployment's project key in
  lowercase, and the check reads that too.
- Your own prose is the usual source, not pasted output: a `Not proven / risk` sentence that says
  whose merged roots the sweep touches first names the key. Write "the deployment's".

## Evidence

#2059 at 1713cb91: the reviewer's review and five review comments each started a `PR Title` run,
all seven red on `body:19: names the private deployment repository or its Dispatch project key`
(the implementer's risk bullet). The body was corrected and all seven re-run (`gh run rerun
--failed`, attempt 2 each): all seven red again, each reading its own event's frozen body. The
first event carrying the corrected body, the round-14 push (ab4eccee), was green (run
39517494021). The same push would have failed `lint`: the tester's `.legion/LEGION-781/test.json:21`
noted "the <project> daemon's" pod on a shared node, the key in lowercase as the pod label spells
it, never linted because every handoff push is trailered; one word redacted to "another
project's" before the push.
