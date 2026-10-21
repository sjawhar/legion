---
title: "Render the artefact and run it: the findings that are not visible in the diff come from executing what the code generates"
category: testing
tags:
  - proofs
  - shell
  - code-generation
  - review
  - smoke
date: 2026-10-21
status: active
related_issues:
  - "INFRA-31"
  - "sjawhar/legion#2085"
---

# Render the artefact and run it: the findings that are not visible in the diff come from executing what the code generates

Legion's runtime writes shell: the install block a role container runs, the start command a
launcher executes, the environment a pod is given. Reading that code tells you what it says.
Running what it produces tells you what it does — and on this issue four findings came out of
running it, none of which is visible in the diff:

- **A cold shared cache is a race, not a cache.** Six role containers of one pod install the same
  plugin pair into one `BUN_INSTALL_CACHE_DIR`. Rendering the block and starting it six times at
  once showed concurrent cold fills corrupting the cache; reading the block showed nothing, because
  nothing in it is wrong in isolation. The fix (`/usr/bin/flock -w <bound>` plus one retry) is then
  obvious, and the test that proves it has to interleave two real installs.
- **A wait of zero is not a wait.** `flock -w 0` is `flock -n`: it returns at 0.00 s with exit 1.
  Measured, not reasoned — which is also why the bound is floored at one second rather than
  allowed to derive to zero on a small configuration.
- **A subshell swallows the exit.** An install that failed inside a command substitution let the
  setup command and the agent start anyway. The behavioural test is "a failing install runs
  neither"; no reading of the rendered text produces that question.
- **A documented delete cannot be run.** `rm -rf` of a Go module cache exits 1 with a
  `Permission denied` line per file (mode 555 on every extracted module) and leaves the expensive
  half behind. The remedy in the docs was written, reviewed and approved before anyone ran it.

Rank this above the subtler lessons about evidence and honesty. It is cheap, it is repeatable by
someone with less judgement than the person who wrote the code, and it does not depend on anybody
being candid under pressure: a rule that survives a tired reader beats a rule that needs a good
one.

The practical form, for a change that generates anything executable:

1. Render the artefact exactly as production renders it (call the renderer from a test or a
   scratch program; do not retype it).
2. Execute it, in the shape production uses — concurrently if production is concurrent, as the
   same user, with the same environment.
3. Execute the failure you are claiming is handled: make the install fail, take the lock away, fill
   the disk, run the delete as the operator would.
