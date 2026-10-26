---
title: "Fix a clear defect where you are, hand any other to the tree that owns the code, and ask only for a decision"
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

# Fix a clear defect where you are, hand any other to the tree that owns the code, and ask only for a decision

The learning above makes every finding outside your own issue an ask, so a person answers each one
before any issue exists. That cost the owner an approval for defects whose fix held no choice of
his, and it made a new issue of work an agent was already standing in. A finding outside your issue
now takes the first of three steps that fits
(`skill://legion-worker/references/findings.md`):

1. **Fix it here.** A defect with a clear fix in code your tree touches or depends on goes into
   your tree's own pull request, while you are in that code. Say in the handoff and the pull
   request body what you fixed beside the issue's own work.
2. **Hand it over.** Any other clear defect goes to the controller with its evidence: post the
   evidence on the report issue, first line `Clear defect from <role>`, and send one Envoy message
   with the defect, the issue it came from, where the fix belongs and the fix. The controller gives
   it to the running tree whose code it is, and replies `Handed dispatch://<KEY>` under the
   evidence. Only when no running tree owns that code does it file a Legion root, with
   `--repo <owner/name>` and `--label legion`, its spec ending `Filed for Legion from <role> on
   <issue>: <evidence>`, and reply `Filed dispatch://<KEY>`. Its triage admits such a root.
3. **Ask.** A finding that needs a person's authority, taste or risk appetite is a `dispatch ask`
   with one recommendation: whether the work is wanted at all, which of several designs, who owns
   it, or a change of scope, cost or risk. File nothing until a person answers. That is the path
   the superseded learning describes, and it still stands for this step.

A fix you are not sure of is not step 1: hand it over, or ask when a person must decide. No role
other than the controller files an issue, and a problem inside your own tree still goes to the
tree's architect.

## Evidence

Four answers the operator gave, each deciding its own case, share the deciding facts this design
follows:

- 2026-10-23, to agents that had filed 78 of a project's 158 issues: "Maybe stop manufacturing work
  I didn't ask for?"
- dispatch://INFRA-49/ask/c7051816-2e86-4cb6-ad56-02ccb01e9355: "This should not have been asked of
  me. You found a clear architectural issue. Fix it. Done."
- dispatch://INFRA-49/ask/45a6f3eb-8be9-49a6-9b55-61dffa69e463: "you can either keep 115 open or
  you can move them to separate roots. This did not need to be an ask for me."
- dispatch://INFRA-49/ask/2d080aec-ce9a-4c7c-a235-ae998d2b0830: "you and your agents keep deferring
  fixes to other issues instead of just fixing it while you're there."

What differs between the first and the rest is the volume of new issues, not whether he is asked.
Step 1 is what holds that volume down: the fix lands in the tree already in the code, so it makes
no issue at all. Step 2 makes one only when no tree owns the code. The split follows
`STRATEGY.md`: humans decide what, agents decide how, and a clear defect's fix is "how" inside work
a person already wants. `skills/legion-controller/SKILL.md` ("Free slots") names a clear defect no
tree owns as the one exception to labelling work nobody asked for, and
`skills/legion-controller/references/finding-asks.md` ("Hand over a clear defect") holds the steps.

Without `--repo`, Dispatch records the project's default repository
(`packages/envoy/internal/dispatch/api/issue_repo.go`) and the daemon admits the issue there
(`packages/daemon/internal/admit/admit.go`), so a defect in a project's second repository would
start work in the wrong workspace.
