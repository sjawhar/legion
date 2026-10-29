---
title: "A coverage check scores what it cannot reduce as reading everything, and reduces only a whole step under an allowlisted environment"
category: github
tags:
  - ci-checks
  - paths-filter
  - fail-wide
  - static-analysis
date: 2026-10-29
status: active
module: .github/scripts
related_issues:
  - "sjawhar/legion#2316"
---

# A coverage check scores what it cannot reduce as reading everything, and reduces only a whole step under an allowlisted environment

Extends docs/solutions/github/a-required-check-asserts-only-what-it-can-decide-without-parsing-the-language-and-refuses-what-it-cannot-classify-INFRA-110.md.

When a check proves that a declaration (a paths filter) covers everything a job reads, its answer
to an input it cannot fully read is not "refuse" but "the job reads everything":

- **Give the unreadable branch the widest set, from the start.** A command the check does not
  reduce counts as reading every file of every kind its tool could compile: for Go, every `.go`
  file, every file under each directory that holds a `go.mod` (testdata and embedded files
  included), and every `go.mod`, `go.sum`, `go.work` and `go.work.sum` of the repository. A
  narrower fallback (the command's module, both modules, `.go` files only) is narrow somewhere,
  and each one was evaded by the next reviewer's probe. A `cd`, `go -C` or moved working directory
  must only be able to widen what a command reads.
- **Reduce only the one form you understand completely.** The form: the step's whole `run:` script
  is the one command line (comments and blank lines aside), in the directory the check computes
  from, with no step `shell:`, no `defaults.run.shell` and no job `container:`. No earlier step of
  the job may write `$GITHUB_ENV` or `$GITHUB_PATH` or run the tool's own config writer
  (`go env -w`). A flag that changes what compiles, such as `-race`, goes into the closure query
  itself (`go list -race`).
- **Read the environment through an allowlist, not a denylist.** Allow only env names the tool
  provably never reads. A denylist misses names that reach the tool sideways: Go reads `GOFLAGS`
  from `$XDG_CONFIG_HOME/go/env`, or else `$HOME/.config/go/env`; `PATH` chooses the binary;
  `BASH_ENV` and `ENV` are sourced before the script.
- **Split the script as its shell does.** Split at `\n` alone, strip only spaces and tabs, and treat
  any character outside printable ASCII, tab and `\n` as not the simple form. Python's
  `splitlines()` and `strip()` also break at `\r` and strip a no-break space, so a hidden command
  reads as a comment to the check while bash runs it. Test this with YAML's own `\r` and `\_`
  escapes: a raw carriage return inside a YAML scalar is normalized by the parser and never
  reaches the check.
- **Write down what the check does not read.** Examples: environment writes made inside local
  actions or repository scripts, and a pinned script's own line splitting. Put that boundary in
  the check's header and its guide, and state the threat it answers (accidental drift, a new
  import). A command hidden on purpose is for review to catch, not for more parsing.

## Evidence

- `.github/scripts/ci_filter_coverage.py` in sjawhar/legion#2316: `module_trees_and_go_files`
  (line 297), `script_reason` (403), `reduce_line` (429), `ENV_ALLOWED` (465), `env_reason` (473),
  `shell_reason` (492), `job_reason` (511); the policy sentence is
  `.github/scripts/check-ci-filters.sh:78`.
- Each narrower fallback was beaten by a red test from the tester or the reviewer:
  - 7cd1a702 took the Run's module only, and a `cd ../envoy &&` line got through.
  - df1f30f5 took every `.go` file, and testdata in another module got through.
  - 75722984 fixed that, and also added the whole-script, shell, container, `BASH_ENV` and
    `-race` rules.
  - 20e98c18 added the shell-like line splitting and the env allowlist.
- On the real tree, a `cd ../envoy` line, `export GOFLAGS=…`, `env: HOME:` and a `shell:`
  override above "Test the Go daemon" each fail the check, naming the command and 400 or more
  files the `test` filter misses.
