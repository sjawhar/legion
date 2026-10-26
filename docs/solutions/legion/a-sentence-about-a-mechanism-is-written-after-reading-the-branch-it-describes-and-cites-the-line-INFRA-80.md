---
title: "A sentence about a mechanism is written after reading the branch it describes, and cites the line in the handoff"
category: legion
tags:
  - documentation
  - agent-read-text
  - review-rounds
date: 2026-10-25
status: active
module: skills, docs/kubernetes.md, packages/daemon
related_issues:
  - "INFRA-80"
  - "sjawhar/legion#2173"
---

# A sentence about a mechanism is written after reading the branch it describes, and cites the line in the handoff

Extends docs/solutions/legion/a-change-that-retires-a-path-or-mechanism-greps-the-old-string-repo-wide-not-the-diff-LEGION-636.md.

That document sweeps a retired *string* out of prose the diff never touched. This is the harder
half: the sentence uses no retired string, names the mechanism correctly, and is still false,
because its author inferred the branch instead of reading it. A grep cannot find it.

- **Read the branch the sentence describes, then write the sentence; put the `file:line` you read
  in the handoff, never in the prose.** An agent-read skill wants the lifecycle, not a citation;
  a reviewer wants the line. A citation that does not support its sentence is a better finding than
  a wrong sentence, because it says the discipline lapsed rather than the reading did.
- **Correcting a condition into an enumeration moves the risk rather than removing it.** The
  condition was wrong about one case; the enumeration asserts every case, so it is wrong about any
  member whose branch nobody read. An enumeration is only as good as its least-read member.
- **One contract expanded in several places is a machine for producing another wrong one.** State
  it once, in the file that owns it, and have the other sites say the short true thing and point
  there.
- **The scope rule is "text this change makes false in a file it owns", not "code the diff
  touched".** The narrow version excludes the whole class: a change rewrites a file without
  touching the two lines its own behaviour has just falsified.

## Evidence (INFRA-80, sjawhar/legion#2173)

Three consecutive review rounds found a sentence the *previous* round had written, each correction
stating the mechanism more precisely than its author had read it:

- A corrected sentence said a merged close deletes the issue's Sandbox with its pod.
  `workflow/linger.go:78-101` sets `except = claim.RoleImplementer` when merged and enqueues the
  `IssueSuspend` only `if !merged`: nothing is deleted, which is *why* the implementer keeps its
  pod for the production check's first look.
- Its replacement said the Sandbox goes "only when a look reports `pending`". `closeCheck`
  (`workflow/check.go:285-296`) enqueues the `issue_close` and the `IssueSuspend` together, so a
  `pass` or `fail` takes it too.
- The enumeration that replaced *that* listed "a look that ends with no report at all" among the
  outcomes that take the pod. `endLook` ends such a look "as a report of pending would", and
  `endLookPending` returns through `startLook` whenever a wake is already recorded
  (`check.go:250-262`), *before* the suspend. The condition is the queued wake, not how the look
  ended — and the member that was wrong is the one nobody had read.

A fourth site stated a single contract four separate times. "Every launch provisions its workspace
afresh" appeared in three documentation passages and one Go comment, and is false for the ordinary
case: `relaunch.go:123-128` keeps a live undrifted pod, `provision` runs only inside the `if
replace` branch at `:166`, and `relaunch_test.go:147-160` pins that a role start does not
reinitialise the issue. Five of an issue's six roles start into the pod the first launch created,
so the text told a reader that an ordinary role start loses unpushed work. The fix that mattered
was not the four edits but the single owner — `docs/kubernetes.md`'s "The disk dies with its pod" —
with the other three pointing at it.

The scope rule earned itself in the same change: a quality pass was told "do not flag unchanged
code" and correctly excluded five true findings under it, every one of them text the change had
made false in a file it rewrote.
