---
title: "An operator-facing sentence outlives what it points at: restate a fact with its fence, its scope and its source, and re-read it against the system it now describes"
category: legion
tags:
  - documentation
  - review-rounds
  - operator-docs
  - evidence
date: 2026-10-21
status: active
module: docs/kubernetes.md
related_issues:
  - "INFRA-31"
  - "sjawhar/legion#2085"
---

# An operator-facing sentence outlives what it points at

**The answer to the seventh correction is a fence, not an eighth correction.** One paragraph of
`docs/kubernetes.md` — the operator's remedy for an issue volume near full — was wrong seven
times across this change's review, and every correction was locally right: each round fixed the
sentence and left the structure that produced it, a hand-maintained list of paths in prose with
nothing holding it to the code. What ended it was `reclaim_doc_test.go`, which derives the
rendered set from the renderer and fails when a path is on neither of the document's two lists.
Everything else below is a habit someone has to keep; this one is mechanical, which is why it is
the part that outlives the people who learned it. A list in prose rots silently: fence it the
first time you correct it, not the seventh.

The rest of this file is why the sentences went wrong in the first place, since a fence checks
structure and not grounds.

Sixteen implementing rounds, seventeen testing rounds and fifteen review rounds went into this
change.
**Eight of the first twelve review rounds turned on a sentence rather than on code, and every one
of those sentences was operator-facing.** The code was right early and stayed right; what kept
failing
was the description of a production system no role in the tree can read. Three of those findings
were in text written to answer the reviewer's own previous finding, and one figure the reviewer had
imported from a neighbouring issue (`20Gi`) caused a round directly and part of another — the
reviewer's own account is that a reviewer who had re-derived his own clause at round nine would
have closed this in ten. The honest number is the useful one.

The four shapes, each with its instance:

- **A restated fact loses its fence.** A number repeated in a second place arrives without the
  condition that made it true: in prose, write "measured on X at T" rather than the figure alone,
  and in a test write the `want` by hand with the configuration it was derived under beside it,
  as `plugins_test.go`'s wait-bound table does. (An earlier version of this bullet cited
  `image_test.go:195-222` as its worked example. The lines resolve, and they argue the opposite:
  that test's `want` is built from the function it tests. A citation that resolves without
  supporting its sentence, in the file that coined the rule — left here as the instance rather
  than quietly swapped.)
- **A figure loses its scope.** "2.1 to 2.7 GiB" means nothing without *of what, where, when*.
  Name the repository a measurement came from: a Go build cache measured on this repository is not
  a bound for a tree whose repository is ten times the size, and a reader two months out cannot
  recover which one you meant.
- **A claim about production needs the fence a derived constant needs.** It names what it rests on,
  so the next reader can tell a measurement from an inference. "2.1 to 2.7 GiB, measured on live
  volumes at 01:45Z" survives; "the duplication is being removed" named nothing and was false.
- **A sentence true of the text above it becomes false when its antecedent moves — without being
  edited.** The sharpest instance arrived after the approval: the reclaim remedy told an operator to
  delete the stale `/legion/scratch/<role>` directories of roles that are not running. That was true
  of the intermediate per-role layout the paragraph above had just measured. The shipped layout
  creates no such directory — the scratch is shared and the only per-role path is a small state
  home — so an operator following it would have deleted nothing, or deleted state homes, while the
  directories that fill stayed untouched. Nothing in the sentence was wrong when written, and no
  amount of re-reading the sentence finds it. Only reading the instruction **against the system it
  now describes** does, which is what a fresh reader did on the fourth correction to that paragraph.
- **A claim you can only assert will drift; one that can be enumerated against the code holds.**
  Three rounds failed on the same list of directories — one missing because its antecedent moved,
  one missing `tmp` and uv, one missing the data home — under sentences of the form "names the
  directories this layout creates", which a reader can only agree with. The version that survived
  says "accounts for every directory this layout creates, either on its delete list or in the two
  exemptions below it", and a seven-line loop over the renderer's paths checks it. Prefer the
  sentence whose truth a script can report on, and write the script into the handoff.
- **A check derived from a claim's shape cannot see a false clause inside it.** That enumeration
  passed at the head where the data home's exemption carried a reason the code refuted — the
  structure was right and only a *reason* was false. It is the same shape as a pin built from the
  value it pins (`installLockWait` made to return a constant left a substring assertion green). So **a claim's
  structure and its grounds need separate checks**: the structural one is mechanical and belongs
  in the handoff; the other is "read the lines the sentence rests on", which nothing automates.
  The tester found it that way, and the grounds it refuted came from the role no one had standing
  to doubt — the architect ruled the exemption *and* supplied its reason, and the reason was the
  one clause nobody derived. Measured afterwards, it was worse than mis-attributed: `ompdirs`
  resolves the profile root at `$XDG_DATA_HOME/omp/profiles/<profile>`, and every Legion role runs
  the named profile `legion`, so the bare `$XDG_DATA_HOME/omp` the sentence made its condition
  decides nothing at all.

So, before a documentation paragraph goes to review: read each instruction as the person who will
execute it, against the current code rather than against the paragraph; and when a paragraph has
been corrected twice, suspect its remaining sentences rather than congratulating the corrections.

## A check's author is the worst-placed party to judge whether it can fail

The sentence above has a second half, and it is the one to search for after writing a test that
turns out not to be able to fail. The control an author imagines is the control their design
already handles: round 20's first version of the enumeration fence was reported green by the
subagent that wrote it, and it was too weak — a path struck from the delete list but still
mentioned inside an exemption's prose satisfied it. The author's own controls passed. What caught
it was a different party running a control of their own choosing, and the one that bit was the
obvious operator move (take a path off the list) rather than the one the design anticipated.

So: have someone who did not write the check choose its mutation, or at minimum choose the
mutation before you look at how the check is implemented. And treat "my check passed its
controls" from the check's author — human or subagent — as a claim to re-run, not a result.

