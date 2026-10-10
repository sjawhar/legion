---
title: "A skill command on issue text in a token-holding session gets a shape, a quoting rule, and a guard on the bare form's absence"
category: skill-patterns
tags:
  - untrusted-input
  - controller-skill
  - skills-guard
  - gh
  - dispatch-cli
  - security-review
date: 2026-10-10
status: active
module: skills/legion-controller
related_issues:
  - "LEGION-668"
  - "sjawhar/legion#1878"
---

# A skill command on issue text in a token-holding session gets a shape, a quoting rule, and a guard on the bare form's absence

- When a skill tells an agent to run a bash command whose argument comes from an issue (a URL in
  `External links:`, a label from `Labels:`, a title) in a session whose `gh` holds an App token,
  the skill states, for that argument: the one shape it may have (`https://github.com/<owner>/<repo>/pull/<n>`),
  how it is quoted in the command (single quotes, exactly as the issue shows it), and what the
  agent does with text that has no such shape or cannot be quoted (skip the issue and name it in
  the summary, never a command argument). Dispatch bounds a label by length alone
  (`packages/envoy/internal/dispatch/api/issue_queries.go`, `normalizeIssueLabels`).
- Enumerate every bash command the skill instructs, with every substitution, in one pass: the
  reviewer found the URL in round 1 and the label three rows down in round 2 of the same file.
- The guard (`packages/pi-legion/src/skills-guard.test.ts`) pins the quoted form and asserts the
  bare form is absent (`not.toContain("gh pr view <url>")`): a positive-only pin lets the removed
  form come back beside the quoted one and stays green.

## Evidence

`skills/legion-controller/SKILL.md`'s pull-request row (`gh pr view <url> …`) and its take step
(`dispatch issue-update … --label <each current label>`) were written at LEGION-668's first head,
the round in which the controller's session first held the review App's full token. Reviewer
round 1 (thread https://github.com/sjawhar/legion/pull/1878#discussion_r4236735170) and round 2
(https://github.com/sjawhar/legion/pull/1878#discussion_r4236923447), fixed in 1b7ab7b123bc and
0b6fa13ed103; the negative controls (unquoting either turns the guard red) are in the tester's
`E2E` line at 4adda6ba.
