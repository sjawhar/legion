---
title: "A forward merge re-reads your prose against the merged code, not only the conflict hunks"
category: legion
tags:
  - forward-merge
  - operator-docs
  - citations
  - documentation
date: 2026-10-29
status: active
module: docs
related_issues:
  - "INFRA-228"
  - "sjawhar/legion#2503"
---

# A forward merge re-reads your prose against the merged code, not only the conflict hunks

When your branch adds prose that describes the code (operator documentation, a skill, a role
prompt) and you merge the base forward:

1. Resolve the conflict markers as usual.
2. Then read every sentence your branch adds again, against the merged tree. Include the
   sentences outside the conflict and those in files that merged clean. The base can change the
   thing a sentence describes without touching the sentence's text.
3. Grep every `(symbol, path)` pair your prose cites, on the merged tree. A name that moved files,
   or now reads in another file, is a citation to fix in the merge commit.
4. Read the base's new wording for the same concept near your text (a renamed component, a merged
   container, a new term), and use it in your sentences.
5. Record the sweep in your handoff as its own proof row: what you grepped and what moved.

The conflict list says where two edits met. It does not say which of your facts the base made
false.

## Evidence

sjawhar/legion#2503 added a paragraph to `docs/kubernetes.md` saying the broker's token is
projected "into a role container". While the pull request was open, main changed the pod model
to one `roles` container with a namespace per role. The conflict was one sentence about
`LEGION_GH_OWNERS_DIR` beside the new paragraph. The paragraph itself merged clean and was now
wrong. The merge commit `c718788b` changed it to "projected for a role", and re-checked the
paragraph's other facts on main (`secrets[dispatchTokenKey]` in `prepare`, the launcher writing
`g<generation>`). They held. The same sweep found two citations that pointed at the wrong file
(`DISPATCH_OIDC_AUDIENCE` is read in `packages/envoy/cmd/dispatch/settings.go`, not `server.go`).
Proof row 7 of that round's `.legion/INFRA-228/implement.json` recorded the sweep.

Neighbouring rules cover text that moved, not facts that changed:
`docs/solutions/legion/a-base-that-moves-a-block-you-edit-leaves-both-sides-intact-and-the-combination-wrong-INFRA-83.md`
and
`docs/solutions/legion/a-forward-merge-also-ports-mains-prose-about-a-component-the-branch-moved-LEGION-247.md`.
