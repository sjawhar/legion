---
title: "Two scripts that classify one kind of string need one imported parser, and the harness must not reimplement the split"
category: best-practices
tags:
  - parsing
  - ci-gates
  - shared-modules
  - test-fixtures
date: 2026-10-30
status: active
module: .github/scripts
related_issues:
  - "sjawhar/legion#2512"
---

# Two scripts that classify one kind of string need one imported parser, and the harness must not reimplement the split

- The moment a second script decides the same thing about the same kind of string, put the grammar
  in one importable module and have every reader import it. A hand-written split in the second
  script disagrees with the first at exactly the inputs the check exists for, and the disagreement
  is silent: each script is self-consistent, and the gap is a class of input neither names.
- Count the test harness as a reader. A fixture that builds its keys with a copy of the algorithm
  under test cannot fail on a wrong split, because both sides move together.
- When the module is the one a gate reads, add it to whatever list says which files that job
  reads, or a change to the grammar alone leaves the job unselected.

## Evidence

sjawhar/legion#2512 found three copies of one reference grammar, one per review round, each by a
reader reading the whole diff rather than by a test.

1. The copy script's four-line split was repeated verbatim in `.github/scripts/mirror-ci-images.test.sh`,
   so the harness could not catch a wrong split.
2. That split stripped only a Docker Hub host, while the gate's `split_reference` strips any host.
   So `registry.example.com:5000/library/golang:1.27.1-alpine` was the `library/golang` row to the
   gate's rules 3 and 7 and **not** a golang row to the copy: the row went past the Go check and
   into the package, which is the one case that check existed to catch. The regression case uses
   exactly that reference, with bytes carrying Go 1.26.3 under a 1.27.1 label.
3. A third reader in `.github/scripts/check-bun-version.sh` split at the first `:`, so it read
   `oven/bun:1.4.4@sha256:…` as the tag `1.4.4@sha256` and refused a row that was correctly
   pinned — the shape `.github/ci-images.yaml`'s own nats row already carried.

The settled shape is `.github/scripts/ci_images.py` (`split_reference`, `upstream_parts`, the
grammar and Docker Hub's host set), imported by all three scripts under
`PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=.github/scripts`, with each harness symlinking the module
into its fixture trees. The gate's own harness was unchanged at 99 assertions across that move,
which is what a refactor should leave.

One shape needed care: `oven/bun:${BUN_VERSION}` is the spelling the repository's Dockerfiles
write, and no tag grammar accepts a `$`. The reader stands the variable in for a string the
grammar accepts and restores it in the tag it returns; a string the grammar refuses answers every
field empty, which each rule reads as naming no release and no row, so a refusal stays a refusal
rather than becoming a pass.
