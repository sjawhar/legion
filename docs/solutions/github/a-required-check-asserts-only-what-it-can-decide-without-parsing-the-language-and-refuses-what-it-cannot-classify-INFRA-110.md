---
title: "A required check asserts only what it can decide without parsing the language, and refuses what it cannot classify"
category: github
tags:
  - ci-checks
  - fail-open
  - assertion-scope
  - test-design
date: 2026-10-24
status: active
module: .github/scripts
related_issues:
  - "INFRA-110"
  - "sjawhar/legion#2138"
---

# A required check asserts only what it can decide without parsing the language, and refuses what it cannot classify

Before writing a check in `.github/scripts/`, write down the question each assertion asks of a
file, and classify it:

- **"What value does this one line declare?"** and **"does this literal appear anywhere in this
  file?"** are answerable with line-anchored patterns and substring scans. Assert them.
- **"Is this region of the file comment text, code, or a string?"** is not answerable without the
  language's own parser. Either hand the file to a real parser, or do not assert it — route that
  criterion to the human reviewer and say so where a reader will look.

A gate that can report agreement it has not established is worse than one that does not try: the
pass is read as proof, and the claim outlives the code that stopped backing it.

Then apply these, each of which this repository already practised somewhere else:

- **Give every read an "I cannot read this" branch, and make that branch a failure.** Enumerate
  the unreadable shapes in the header *before* writing the body, as `check-bun-version.sh:19-41`
  does; raise and report them as `check-image-trigger-paths.sh`'s `Unreadable` does at its six
  raise sites. An approximate lexer is acceptable only when what it cannot read leaves coverage
  and becomes a refusal, never a pass (`check-release-credentials.sh:189-193`).
- **Add a vacuity guard to any scan that can come up empty** — "covered 0 of N, so it proves
  nothing" (`check-zod-version.sh:106-109`, `check-bun-workspace-manifests.sh:157-159`,
  `check-image-trigger-paths.sh:1031-1034`), with a suite case asserting its wording.
- **Prefer assertions with no false-failure mode.** "Some line of this file names both constants"
  is monotone: adding text can only make it pass. That is what makes it safe in front of every
  pull request. Record its limit honestly too — it pins no particular row, so an unrelated line
  satisfies it.
- **Write the adversarial cases from the grammar, first, not from the happy tree.** For any check
  whose misclassification yields a *pass*, the hostile-but-compiling input is the test; a fixture
  builder that only produces well-formed trees proves the easy half.
- **When a plan pins the runtime, it has already decided which assertions are possible.** Resolve
  that conflict in the plan. Standard-library-only Python before the Bun install forecloses a
  TypeScript or Go parser, so it forecloses every assertion that needs one.
- **An acceptance criterion whose subject is prose belongs to the reviewer; get that ruling before
  the implementer writes code.** A plan decision marked "the architect may overrule this" is not a
  licence to proceed — it is a question.

## Withdrawing an assertion

When an assertion comes out, it comes out in one push, with every text that claimed it:

- the success line, the script header, the suite header, each guide row, any
  `docs/solutions/` document that cited it, and the pull request body;
- keep every case whose fixture is still a hostile input for the assertions that *remain*,
  re-pointed at the surviving behaviour rather than deleted with its subject;
- name every deleted case with its reason in the handoff, so nobody has to diff for it;
- the withdrawal is complete when a grep for the removed symbols **and** a grep for the removed
  claim's wording both come back empty, run by someone other than the author.

## Evidence (INFRA-110, sjawhar/legion#2138)

The change added `.github/scripts/check-body-embed-limit.sh`, holding `DISPATCH_BODY_MAX`
(`packages/contracts/src/dispatch-tools.ts`) at or below `maxInputChars`
(`packages/envoy/internal/dispatch/embed/embed.go`), since a body longer than the embedder's input
limit has its tail dropped from search by meaning with no error.

It shipped with two halves and five review-or-test rounds between them:

- **The numeric half** — two line-anchored declaration reads, each fatal on no match or on more
  than one, and one comparison — was correct in round 1 and never changed.
- **The comment half** — "the comment attached to each declaration names the other constant" —
  produced four defects, three of them fail-open, each found after someone with good reasons had
  judged it correct:
  1. a conforming comment *refused* (three shapes: an unprefixed `/* … */` block, a Go doc comment
     on a one-member `const (` group, a trailing same-line comment) — fail-closed;
  2. a `//` line whose text ended in `*/` read as a block close, crediting a mention attached to
     nothing (tester's red test, CI red at `c28c6dd7`);
  3. an upward climb that stopped at *some* `/*` above rather than the one that opened the `*/` it
     saw, crediting a neighbouring declaration's block and three lines of executable code;
  4. an unterminated `/*` inside a template literal classifying every line below it as comment
     text (tester's red test, round 4).

Each fail-open printed `and each names the other` for a tree where it was false, while the numeric
gate kept firing correctly — so nothing ever truncated silently, and what failed was the claim.

The class, not the instances, was the problem. The narrow remedy proposed after defect 4 (credit
only blocks whose close is found) was implemented, and then shown still to credit a template
literal that closes on the same line as its own comment-token text — a tree that compiles — so it
was reverted. No rule that tracks no quote characters closes that shape, and tracking quotes
refuses the real file, whose comments carry apostrophes. That is the point at which the assertion
was withdrawn rather than patched a fifth time.

What shipped: the numeric gate, and one whole-file scan per package guide requiring a line that
names both constants. The declaration comments themselves are unchanged and are confirmed by a
human at review; the script's header, both guide rows and
`docs/solutions/envoy/a-dispatch-body-cap-is-spent-against-budgets-in-other-units-ask-which-refuse-and-which-truncate-INFRA-45.md`
each say which half a build holds and which a reviewer does. The suite went from 85 assertions
over 33 cases to 49 over 18, every deleted case named with its reason in the issue's implement
handoff.

Related: `docs/solutions/daemon/scrub-tmux-environment-tables-by-parsing-show-environment-s.md`
states the same discipline for a daemon reader ("a docstring that promises what the code cannot do
… either widen the code or narrow the claim"), and
`docs/solutions/best-practices/a-long-line-a-worker-read-through-a-truncating-window-comes-back-truncated-check-its-byte-count-against-the-fork-point-LEGION-636.md`
records an approximate lexer breaking on quoting it could not see. A search of `docs/solutions/`
for the *failure vocabulary* — fail-open, parser, classify — rather than only for the feature's
own prior issue would have surfaced both at plan time.
