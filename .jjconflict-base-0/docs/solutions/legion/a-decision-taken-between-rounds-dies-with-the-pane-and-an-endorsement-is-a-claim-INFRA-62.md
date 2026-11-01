---
title: "A decision taken between rounds dies with the pane, an endorsement is a claim like any other, and a number changed in one place is a search of the file"
category: legion
tags:
  - review-rounds
  - envoy-messages
  - handoffs
  - workspace-recreated
  - documentation-drift
date: 2026-10-21
status: active
module: skills/legion-worker
applies_when:
  - An architect or reviewer settles something between two review rounds
  - A pane is relaunched or its workspace recreated, and the re-sent task carries an older round's text
  - A review endorses a passage rather than finding fault with it
  - A number the code renders is changed and the repository documents it in prose
related_issues:
  - "INFRA-62"
  - "sjawhar/legion#2089"
---

# A decision taken between rounds dies with the pane, an endorsement is a claim, and a changed number is a search of the file

- **Write a between-rounds decision into a durable artefact, not only into a message.** A role's
  Envoy history does not survive its process; a relaunched pane rejoins with the committed handoffs
  and the task text it is re-sent, which may be an older round's. A decision that exists only in
  chat therefore reaches a worker that was alive for it and nobody else. The reviewer or architect
  who takes it puts it in the next review body and in `review.json`; one that overrides something a
  committed handoff already records says so there, because the handoff is what a recreated pane
  reads.
- **After a recreation, or when the task text and the committed handoff disagree with anything you
  were told, ask the owning role which is current before acting.** The committed file is the right
  default and still a snapshot of the moment it was written. One message costs less than a round.
- **An endorsement in a review is a claim and belongs in the same evidence bracket as a finding.** A
  reviewer who agrees with a passage's conclusion stops checking its reasoning, and the endorsement
  then outlives the error: a second reader trusts "I checked this and it is right" more than the
  passage itself.
- **A number changed in one place is a search of the file, not a read of the section you are
  editing.** Prose is guarded by nothing — a golden holds the rendering, not the paragraph. After
  changing a rendered value, grep the repository for every spelling of the old one, including the
  tables above the paragraph you were editing.

## Evidence

INFRA-62 took five review rounds. Every finding in all of them was a document or a test asserting
something the code did not do.

- At 04:22Z an architect decided, between rounds, that a mirrored constant should be deleted once
  its guard existed. The implementer's process died around that time, its workspace was recreated,
  and the task it was re-sent carried round 1's text; the only durable copy it could read was the
  previous round's `review.json`, where the same item stood as the reviewer's *batched maintainability
  minor*, which is what it was before the override. The decision reached the worker twice by Envoy
  and arrived neither time. It landed a round later, from a review thread — the durable route.
- In round 2 the reviewer wrote that a code comment "gets it right" about the Kubernetes termination
  ordering. The comment had the ordering backwards, and the reviewer read past it because its
  conclusion was the one it wanted; the thermonuclear pair caught it a round later. The reviewer
  recorded that as its own error rather than the implementer's: *a review-process page whose
  mistakes all belong to someone else teaches nothing a reviewer can use.*
- A resource table seventy lines above the paragraph under discussion still documented the pod's
  grace period as 30 after round 2 raised it to 35. Three roles — implementer, tester and reviewer —
  argued the paragraph across three rounds without opening the table. It was found in round 4, by a
  pass that read the file rather than the diff.
