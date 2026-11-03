---
title: "An inventory of code sites in a pull-request body carries its head and its search, and every 'needs nothing' reason is re-derived"
category: legion
tags:
  - review-rounds
  - pr-body
  - evidence
  - forward-merge
date: 2026-11-02
status: active
module: skills/legion-worker
related_issues:
  - "sjawhar/legion#2648"
---

# An inventory of code sites in a pull-request body carries its head and its search, and every "needs nothing" reason is re-derived

When a reviewer or an architect asks for a list of code sites in the pull request's body — every
place that mints a credential, writes a file, touches a table — the list is evidence, and nothing
in the repository holds it true. Three rules make it worth reading:

- **Print the search that produced it and the commit it was read at.** The search makes the list
  falsifiable: a site it omits is a hit of that search with no row. The commit tells the next
  reader whether it has been re-read since.
- **Regenerate it at every push and at every forward merge**, not only when a row's content
  changes. A merge moves line numbers and can add a site the list never saw.
- **Treat each "needs no change, because …" row as a claim to re-derive from the code, and put
  that reason in the code.** The enumeration is the easy half; the reasons are where a defect
  hides, and a reason in a comment survives the merge and reaches the next reader of the
  function, which the body does not.

## Evidence

sjawhar/legion#2648's architect asked for a list of every place the Legion daemon mints a GitHub
App token or calls GitHub for a recorded repository, each row saying whether it is mapped through
the rename equivalence or why it needs no mapping, and said the reviewer would judge that list as
a whole. All three failure modes above then fired in it.

**The reason was the defect, not the enumeration.** The round-4 audit searched the daemon with
thirteen patterns and found no site missing — and listed `reviewAppClient` under "needs neither"
with the reason that its repository comes from `review.Repo`, GitHub's own event, "already the
name GitHub holds". That reason had been falsified by this pull request's own round-2 fix: once
`heardRepositories` subscribes to both names of a pair, a review GitHub published before the
transfer and processed after it arrives under the old one. It was round 5's only finding, with an
unbounded redelivery loop behind it, because the body read is an error that intake naks.

**A merge of main falsified the list, and that cost a round that moved no code.** The tester at
the merge head measured four of the listed line numbers landing on the wrong construct, and
eleven more had moved; the list's header still named the pre-merge head. Round 6 was spent
re-stamping it. One round earlier, re-reading every number after a single push had already found
eight wrong where the review had named two.

**The exemptions that survive are the ones in the code.** `reviewAppClient`'s doc comment now
says why the mapping is needed and why a post-transfer event is unaffected; `implementTokens` and
the tmux provisioning mint say at the site why the token follows the rename while the address
does not. Those sentences are in the merged tree. The body's tables are not.
