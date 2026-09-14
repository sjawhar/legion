---
title: "Answer 'can the extension do it?' by citation at the pinned tag, and name an upstream-bound fork setting for the upstream, not for Legion"
category: legion
tags:
  - oh-my-pi-fork
  - knives
  - forbidden-names
  - extension-api
  - planning
  - omp-fork-pin
date: 2026-09-14
status: active
module: sjawhar/oh-my-pi feat/session-sql-storage; packages/daemon/src/daemon/omp-pin.ts
related_issues:
  - "LEGION-80"
  - "LEGION-5"
  - "sjawhar/legion#1081"
---

# Answer the hook question by citation, then name the setting for its destination

## Two decisions that shaped the whole issue

Sami's order of attempts for putting an agent's session in Postgres was: the Legion extension
first (swap the storage from inside OMP), a setting in the Oh My Pi fork only if no hook exists.
Two things decided early saved every later phase from re-litigating them.

### 1. "Is there a hook?" is answered by code citation at the pinned tag, not by trying

The planner opened a knives workspace at the exact commit `OMP_FORK_PIN` names (not whatever the
fork checkout happened to be on), and cited: `SessionManager`'s storage is a `readonly #storage`
assigned in a private constructor; every factory takes the storage as an argument defaulting to
a fresh `FileSessionStorage`; the CLI constructs the manager before extensions load; the
extension API exposes nothing storage-shaped; `ExtensionContext.sessionManager` is a read-only
`Pick` of getters. Six citations with file and line, each re-openable at the tag. That turned
"option 2 is not possible" from an opinion into a fact the architect could record in the spec's
Decisions section — and nobody spent an implementer's day on a monkey-patch that
`instanceof FileSessionStorage` checks would have broken anyway.

Do this whenever a plan hinges on "the platform lets us / does not let us": cite the sealed
field, the construction order, and the API surface at the pinned version. Rejected workarounds
belong in the plan too, with the line that breaks them.

### 2. A fork change bound for upstream carries no Legion names

The spec first named the pane variable `LEGION_SESSION_DSN_FILE`. Sami's knives registry for the
fork lists `legion`, `envoy`, `secretsd`, `dispatch://`, and `knives` as **forbidden** in an
upstream-bound diff ("internal names an upstream-bound diff must not add"), and his own answer
named an upstream pull request as the destination. So the fork setting became generic —
`session.storage` (`file` | `sql`) and `session.sql.dsnFile`, with `OMP_SESSION_STORAGE` and
`OMP_SESSION_SQL_DSN_FILE` overriding them, following the fork's existing `auth.broker.*`
pattern — and `LEGION_SESSION_DSN_FILE` was not delivered anywhere: a second name for one file
that nothing would read. `knives audit`'s `forbidden` scan (`[]` for the branch) is the check;
run it before the push, and keep issue references in the knives notch and the Legion PR only.

When a spec names a variable the fork must read, ask first whether the fork change is going
upstream. If it is, the name is the upstream's to give (its prefix, its settings namespace), and
the spec changes — the architect records why under "New since we talked", not the implementer
in a commit message.

## Operational facts worth knowing before the next fork change

- The fork checkout is knives-managed (`cd ~/oh-my-pi && knives start <branch> --why …`;
  `knives finish` the moment your active work stops). A new branch starts on the **sami
  release's shared base**, not on the pinned tag, so every line number in the plan shifts —
  re-locate by symbol.
- A worker's GitHub token pushes to `sjawhar/oh-my-pi` (the implement App's installation covers
  it) but cannot open a pull request on `can1357/oh-my-pi` or cut a release. Push with the Legion
  credential helper for that one command
  (`GIT_CONFIG_COUNT=2 GIT_CONFIG_KEY_0=credential.https://github.com.helper GIT_CONFIG_VALUE_0= GIT_CONFIG_KEY_1=credential.https://github.com.helper GIT_CONFIG_VALUE_1='!legion credential' jj -R <ws> git push --bookmark <branch>`,
  dry-run first, from your own bash call — the grant is minted per command). The upstream PR
  and the release cut are the operator's; the contributor sentence the fork's CONTRIBUTING.md
  requires is Sami's to write, never generated.
- The operator was on the critical path twice for release cuts and once for the corrective fork
  commit itself, because the worker's pane changed user mid-issue (uid `ubuntu` → `legion`) and
  `/home/ubuntu/oh-my-pi` became unreadable (LEGION-147). Write the fork change you need into the
  handoff precisely enough that someone else can make it (file, exact code shape, test, commit
  message, changelog line): that handoff spec is what the operator applied verbatim.
- Verify a release carries your commit **by blob sha** before pinning it
  (`legion gh -- api 'repos/sjawhar/oh-my-pi/contents/<path>?ref=<tag>' --jq .sha` against the
  same at your commit) — the operator rebased the branch, so commit ids differed while content
  did not; and the tag may exist before anyone reports it (`legion gh -- release list`).
- A fork branch alone (upstream base) does not carry the sami release's other members, so
  `legion probe-image` against a from-source build of the branch fails its **first** probe
  (`pi.agents` missing) — run the third probe alone against the branch source, and the full
  `probe-image` only against the tagged release.
