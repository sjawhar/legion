---
title: "A superseded mechanism does not retire its rule, and a gate that reads shell is one pass derived from bash and proven against bash before the first review round"
category: legion
tags:
  - bash-tokenizer
  - shell-metacharacters
  - docs-solutions
  - superseded
  - review-rounds
  - regression-chain
  - ground-truth
  - check-release-credentials
date: 2026-10-15
status: active
module: .github/scripts/check-release-credentials.sh
related_issues:
  - "LEGION-748"
  - "sjawhar/legion#1969"
  - "LEGION-45"
  - "LEGION-631"
---

# A superseded mechanism does not retire its rule, and a gate that reads shell is one pass derived from bash and proven against bash before the first review round

Extends docs/solutions/legion/shell-command-gates-derive-from-bash-word-splitting-not-example-forms.md.
That document already held the rule this issue re-derived over four review rounds — derive the
tokenizer from bash's metacharacter table, cover the quoting contexts as a matrix, prove every
row against bash — but its frontmatter says `status: superseded by LEGION-631`, because the jj
operation-log gate it served was deleted, and nobody read it. The delta here is three rules.

## The rule

1. **A document's rule outlives its mechanism.** When the code a learning served is deleted,
   mark the mechanism dead in a sentence under the H1 and leave `status: active`, or write the
   rule into a successor the old file names with `supersedes:`; never let a `status: superseded`
   stand for a rule that still applies. Searching `docs/solutions/`, read a superseded hit to
   the end before discarding it: the status describes a mechanism, the body may describe your
   problem.
2. **A gate that decides what a shell would run is one quote-aware pass, written before the
   gate's first rule.** Walk the text once with bash's own state: the current quote (`'`, `"`,
   none), a stack of `$(…)`/backtick contexts that suspend and restore it, whether the next
   character begins a word. A `#` opens a comment only at word start — run start, after unquoted
   whitespace or a newline, after an unquoted `;`, `&`, `|`, `(`, `)`, `<`, `>` or backtick, and
   never after `{` or inside quotes — and the comment runs to the real newline, so a backslash
   inside it continues nothing; backslashes are read one at a time, so an odd run before a
   newline continues the line and an even run does not; quotes carry across newlines; a
   redirection's target is not a word. Two passes that each know half of this (strip comments
   per line, then join continuations; or the reverse) disagree about where a word begins and
   whether a `#` is quoted, and every disagreement is a shape that runs in bash and passes the
   gate.
3. **Prove each shape against the interpreter, and prove a regression chain by replacing the
   model.** The ground truth for "does this body run `bun`" is `printf '%s\n' "$body" | bash`
   with a logging stub on `PATH` (or `echo RAN`), run beside the gate's own function on the same
   bytes, before the shape goes into a fixture. When a review finding is a regression inside the
   previous round's fix, or a thread the reviewer resolved is reopened, the next commit replaces
   the model and re-derives every earlier shape against it; patching the reported shape adds a
   round. The signal is cheap to read: the architect's "every round since 2 has been fixing what
   the previous fix added" was true two rounds before it was said.

## Evidence

- `.github/scripts/check-release-credentials.sh` at sjawhar/legion#1969's approved head: the
  `Scanner` class is the one pass; its header paragraph "Command position is found by one
  quote-aware scan …" lists the rules above, and a threat-model paragraph names what the gate is
  for (drift, not an adversarial workflow edit) and the pass-through class it accepts on that
  ground.
- The rounds that re-derived the rule: tester round 1 (`shell: python` bodies and interpreters
  over a `scripts/` program unread); review round 3 (a `.replace("\\\n", " ")` before comment
  stripping let `# keep the toolchain pinned \` swallow the next line's `bun install`); review
  round 4 (per-line comment stripping reset quote state at newlines and knew only whitespace and
  line start as word starts: `;# … \`, a `#` opening the second line of a single-quoted string,
  `echo a\ #; bun install` and `echo a\` + newline + `#;bun install` all passed; two of them
  had been refused at the previous head). Round 6's rewrite pinned 29 shapes, each checked
  against real bash first (8 of 8 agree in the PR body's round-6 proof), and the review that
  followed found nothing.
- The pre-existing document's rules 2-4 (metacharacter table, quoting matrix, prove against
  bash) are the fix verbatim. Its `status:` line reads "superseded by LEGION-631 — the pane-rule
  mechanism this tokenizer served … is deleted whole"; the planner's gap analysis, the
  implementer's search and the reviewer's rounds all missed it.
- The tester's round-5 note records the generator-side failure the proof rule guards against:
  two round-4 backslash probes were built through a Python string whose own line continuation
  had already joined the lines before the YAML was written, so both probes exercised one-line
  commands and were green while the regression they meant to cover stood.
