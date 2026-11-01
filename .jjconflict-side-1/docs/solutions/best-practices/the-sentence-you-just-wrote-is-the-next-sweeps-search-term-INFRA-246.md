---
title: "The sentence you just wrote is the next sweep's search term"
category: best-practices
tags:
  - documentation-drift
  - sweep
  - code-comments
  - corrections
date: 2026-10-25
status: active
module: packages/pi-envoy
related_issues:
  - "INFRA-246"
  - "sjawhar/legion#2164"
---

# The sentence you just wrote is the next sweep's search term

Extends docs/solutions/best-practices/a-retired-model-is-swept-by-belief-then-by-symbol-INFRA-66.md.
That document says to sweep a retired model by belief and then by symbol. The delta here is a
third axis it does not cover, and the structural answer that makes all three cheaper.

- **Sweep for the corrected sentence's own wording, not only the retired model's.** A copy already
  phrased the way you are about to phrase the fix cannot match a pattern set built from what you
  are deleting: the string was never in it. After you write the correction, grep the tree for the
  words you just wrote and read every hit.
- **Search in units the file is written in.** A claim wrapped across two comment lines matches no
  single-line pattern. Search the phrase's distinctive two or three words separately, or search
  multiline, before concluding a sweep is complete.
- **A sweep hit is not a defect: read each one.** The same words are correct elsewhere. "Accepted
  registration" is wrong for a ledger promotion that happens before the request is issued and
  exactly right for the clock that times the last registration the listener accepted; six of nine
  hits were the second kind.
- **State an invariant once, at the declaration it governs, and point at it from everywhere
  else.** Four copies of one sentence is what made three sweeps necessary. A pointer that names
  the symbol (`see abandonedHolds`) is found by grepping the symbol, which no paraphrase is.
- **Never hand-type a number into a pull request body.** Paste the command's output. A count
  carried over from a working copy that had since changed survived two rounds and was caught by a
  human reading, because nothing executes a number in prose.

## Evidence

Rounds 7 to 9 of one pull request. Round 7 rewrote how a plugin keeps role holds; round 8
corrected three descriptions of the replaced model and swept for that model's phrases; round 9
existed because a fourth copy, already written in round 8's own corrected wording and wrapped
across two comment lines, could match neither that sweep's patterns nor a single-line search for
the new one. Round 9's sweep searched both wordings over every `.md`, `.ts`, `.go` and `.sh`
outside `node_modules`: ten hits, nine of them accurate and each read rather than counted.

A sixth correction in the same tree was a test count in the pull request body — 854 where the
suite ran 853 — taken while a case that was later removed still sat in the working copy. Every
one of the six was found by a person reading text against code.

A technique worth keeping from the same rounds: to show a prose-only round changed no behaviour,
strip comment lines from both heads of the file and compare the result byte for byte. It is
stronger than reading the diff and it takes one command.
