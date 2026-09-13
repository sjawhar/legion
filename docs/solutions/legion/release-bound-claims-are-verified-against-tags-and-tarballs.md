---
title: "A claim that release X.Y.Z is the first to contain a fix is verified against the tag graph and the published tarball, never against a review thread"
category: legion
tags:
  - release
  - npm
  - tags
  - jj
  - verification
  - pi-legion-envoy
  - review-threads
  - docs-accuracy
date: 2026-09-13
status: active
module: packages/pi-envoy, docs
related_issues:
  - "LEGION-52"
  - "sjawhar/legion#1018"
  - "LEGION-54"
  - "sjawhar/legion#992"
  - "sjawhar/legion#993"
symptoms:
  - "a CHANGELOG or AGENTS.md sentence names the first release that carries a behaviour, and the tester's tag/tarball check says the bound is off by one"
  - "two pull requests merged minutes apart and the release workflow cut a tag between them"
---

# A claim that release X.Y.Z is the first to contain a fix is verified against the tag graph and the published tarball, never against a review thread

## Context

LEGION-52 (#1018) documents the daemon/plugin contract and had to say which plugin releases write
the credential file `LEGION_GRANT_FILE` (LEGION-54, #992). The reviewer's thread stated, as a
premise for a correction, that "1.20.0 was released 06:06Z from a commit that contains #992". The
implementer copied the bound into the daemon `AGENTS.md` and the pi-envoy `CHANGELOG.md` as
"1.20.0 through 1.22.2 are LEGION-54 builds". The tester's next round failed the PR on that one
fact: 1.20.0 does not contain #992. The bound is 1.20.1.

## What actually happened at release time

`release.yaml` cuts a `chore: release pi-envoy vX.Y.Z [skip ci]` commit and tag on every merge to
`main` that touches the package. On 2026-09-13 two pull requests merged two minutes apart:

| event | commit | committer time (UTC) |
| :--- | :--- | :--- |
| #992 (LEGION-54, the credential file) merged | `b3f5870a944c` | 06:04:08 |
| tag `pi-legion-envoy-v1.20.0`, cut for #993 (LEGION-33) | `384dc5ee6629` | 06:06:12 |
| tag `pi-legion-envoy-v1.20.1` | `6312ef9791d0` | 06:09:10 |

The 1.20.0 release commit's parent chain goes through #993, not #992: the release workflow run
for #993 had already checked out its merge commit when #992 landed. So a release cut *after* a
merge, by clock, does not necessarily *contain* that merge. Version numbers order releases; they
say nothing about which pull requests are inside.

## The two checks, and what each proves

Both are cheap and neither needs a checkout of the release.

1. **Tag ancestry** — is the fix's merge commit an ancestor of the release tag?

   ```sh
   jj -R "$WS" log -r 'pi-legion-envoy-v1.20.0 & descendants(b3f5870a944c)'   # empty → not contained
   jj -R "$WS" log -r 'pi-legion-envoy-v1.20.1 & descendants(b3f5870a944c)'   # one commit → contained
   ```

   (`git merge-base --is-ancestor b3f5870a944c pi-legion-envoy-v1.20.0` is the same question.)
   This proves what the *source* at the tag contains.

2. **The published artifact** — does the tarball people actually install carry the behaviour?

   ```sh
   npm pack @sjawhar/pi-legion-envoy@1.20.0 && tar -xzf sjawhar-pi-legion-envoy-1.20.0.tgz
   grep -c LEGION_GRANT_FILE package/dist/legion.js     # 0 for 1.20.0, 4 for 1.20.1
   ```

   This proves what the *build* shipped; a tag can contain a fix whose bundle step dropped it, and
   the tarball is what the gate in `verifyLegionPluginContract` will read the manifest of. For a
   manifest field rather than bundled code, `npm view @sjawhar/pi-legion-envoy@<v> legion` answers
   without downloading (that is how the "1.14.0 is the first release to declare
   `legion.daemonApiVersion`" bound in the same document was checked, one version at a time).

Do both when a sentence names a version as a bound. The tag check alone would have caught this
case; the tarball check is what catches the case the tag cannot.

## The discipline

- A factual claim in a review thread — the reviewer's included — about which release contains
  what is a hypothesis, not evidence. The implementer who copies it into a document owns it and
  verifies it first. Here the reviewer's premise was wrong, the implementer's copy of it was wrong,
  and the tester was the first to run either command.
- Write the bound with its evidence beside it in the commit message (the two commands and their
  outputs), so the next person who doubts the number can re-run them instead of re-deriving.
- When correcting a bound, also correct every dependent clause: "only a release before 1.20.0
  …" moved with it, and the daemon `AGENTS.md` and the CHANGELOG had to say the same thing (the
  constant's doc comment and the pi-envoy `AGENTS.md` never carried the parenthetical — grep all
  the places before declaring it fixed).

## Related

- `../daemon/plugin-daemon-api-contract-version-gate.md` — the contract this bound belongs to,
  and why a package-version lower bound was rejected as a *gate* (the same fact — a version number
  does not say what a release contains — from the other side).
- `grant-delivery-plugin-omp-contract.md` — what LEGION-54's credential file replaced, and the
  incident that is often confused with a contract skew.
