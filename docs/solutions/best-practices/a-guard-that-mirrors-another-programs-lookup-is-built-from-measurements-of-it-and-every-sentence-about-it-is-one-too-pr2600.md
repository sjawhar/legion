---
title: "A guard that mirrors another program's lookup is built from measurements of that program, and every sentence about it is a measured claim"
category: best-practices
tags:
  - guard
  - refusal
  - third-party-behaviour
  - error-message
  - table-test
date: 2026-10-31
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2600"
---

# A guard that mirrors another program's lookup is built from measurements of that program, and every sentence about it is a measured claim

When your code refuses something because of what another program would do with it, the other
program's behaviour is the specification, and you do not know it until you have run it.

- **Enumerate the entry kinds that program can meet and measure its answer for each** before you
  choose a predicate: a directory, a regular file of the same name, a symlink to a directory, a
  component that is not there.
  One row of a table test per kind, with the measurement in the row's comment.

- **Prove the guard in both directions.**
  That it refuses what the program would act on, and that it still lets through what the program
  ignores. Over-refusal is the worse failure: it stops work that was always safe, and no test that
  only looks for refusals will find it.

- **Pin the predicate with a mutation control.**
  Change the predicate to the plausible wrong one, run the table, and record that the new row — and
  no other — fails. A row that passes under both predicates pins nothing.
  (`docs/solutions/testing/mutation-proof-probe-tests.md` is the general form of that control.)

- **Every sentence of a comment, an error message or a guide that describes that program is a
  factual claim.** Measure it on the pinned version, name that version in the text, and state it in
  one place the others point at. A claim repeated in seven places is re-measured in seven places,
  and in review each copy is its own finding.

- **A claim that holds only in one state says so.** "It writes X" and "it writes X when Y" are
  different claims, and the first is false the moment a reader meets a case without Y.

## Evidence (sjawhar/legion#2600, INFRA-623)

The guard refuses a `jj` command that creates a repository when a `.jj` sits above the directory it
would run in. Everything below was measured on jj 0.45.1.

- `os.Lstat` on the entry was wrong **twice**, and in opposite directions. It refused what jj
  ignores — below a directory holding a *regular file* named `.jj`, jj answers
  `Error: There is no jj repo in "."` — and it would have missed what jj opens: with `workspace/.jj`
  a symlink to a repository's `.jj` directory, `jj log -r @` run in `workspace/state` printed a
  commit id. `os.Stat` with `IsDir()` matches both. The first of these shipped for a round with a
  table row that locked the wrong answer in, the row's own comment asserting that a `.jj` file is
  "jj's own workspace pointer form" — it is not; the pointer is the file `.jj/repo` *inside* a `.jj`
  directory.

- The symlink row only arrived when the reviewer noticed that moving from `os.Lstat` to `os.Stat`
  had made the case load-bearing with nothing holding it. The control: with `os.Stat` changed back
  to `os.Lstat`, that row alone fails —
  `refuseEnclosingWorkspace(…/workspace/state) = no error, want a refusal naming …/workspace/.jj` —
  and passes again with `os.Stat` restored.

- Three prose claims shipped false or unconditional and were corrected by running jj:
  "jj would open the enclosing repository **instead of** creating its own" — it does both:
  `jj git init --colocate c` inside a repository `b` migrated `b`'s legacy configuration, wrote
  `b/.jj/repo/config-id` **and** printed `Initialized repo in "c"`.
  "the run writes the enclosing repository" — only when that repository still holds a legacy
  `.jj/repo/config.toml`; a fresh 0.45.1 repository's `.jj/repo` listing was identical before and
  after. That one claim stood in **seven** copies across the code, its tests and `docs/testing.md`.
  "`-R` keeps jj off the repository its working directory sits in" — false: `jj -R <b> log` run
  inside a repository `a` that still held a legacy `config.toml` migrated it, writing
  `a/.jj/repo/config-id`.

- The surviving text names the version each time it states one of these facts.
