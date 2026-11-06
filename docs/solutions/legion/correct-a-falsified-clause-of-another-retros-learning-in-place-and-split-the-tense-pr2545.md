---
title: "A change that falsifies one clause of a standing learning corrects that clause in place, and splits the tense"
category: legion
tags:
  - learnings
  - retro
  - documentation
date: 2026-10-30
status: active
module: docs/solutions
related_issues:
  - "sjawhar/legion#2545"
---

# A change that falsifies one clause of a standing learning corrects that clause in place, and splits the tense

- When your change makes one sentence of another issue's `docs/solutions/` document false, correct
  that sentence in the pull request that falsified it. Do not write a `supersedes:` document for
  one clause, and do not leave the false sentence for a later reader.
- The never-edit rule in `skills/legion-retro/references/learnings.md` binds the retro step, not
  every change: it exists so two retros landing in the same hour do not edit the same lines.
- Read each conjunct of the sentence against the code before you move its tense. A sentence that
  joins two facts can have one fact that your change retired and one that is still live, and a
  blanket past tense retires a live rule by accident.
- Say what holds now, beside the history. An evidence line that names a fixed defect keeps its
  commit and its date; the clause about what an instruction says gets the instruction as it is.

## Evidence

sjawhar/legion#2545 renamed the changelog fragment the repository's instructions teach.

- `docs/solutions/best-practices/a-scanner-refuses-every-entry-it-cannot-handle-and-the-remedy-its-error-names-is-tested-too-INFRA-629.md:38`
  read "Fixed in 6b255451bb62: fold-plan refuses them by name, and `AGENTS.md` says to write each
  `/` as `-`." The second half became history with that change; the first half is live behaviour,
  `.github/scripts/release-push.sh:832` with its refusal message at `:857`. The edit keeps
  "fold-plan refuses them by name" in the present, puts the `AGENTS.md` clause in the past, and
  adds one sentence naming what `AGENTS.md` says now. A whole-clause past tense would have told
  the next reader that the fold no longer refuses a bad entry.
- `skills/legion-retro/references/learnings.md:3` scopes that file to step 3 of
  `skill://legion-retro`, and its never-edit section at lines 6-10 gives concurrent retros as the
  reason. #2545's plan had to settle the conflict before the implementer would touch two learning
  documents written by other retros; each edit stayed inside the one stale sentence and wrote no
  superseding document.
