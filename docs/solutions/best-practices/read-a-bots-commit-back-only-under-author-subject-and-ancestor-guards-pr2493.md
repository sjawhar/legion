---
title: "Read a bot's commit back only under author, exact-subject and ancestor guards"
category: best-practices
tags:
  - git
  - trust-boundary
  - release-workflow
  - shell
date: 2026-10-29
status: active
module: .github/scripts
---

# Read a bot's commit back only under author, exact-subject and ancestor guards

## The rule

1. When automation records in a commit subject what it did (for example "rebuild from <sha>"), a
   later run trusts that record only when all three of these hold:
   - the commit's author is the bot;
   - the subject matches an anchored regular expression exactly;
   - the commit the subject names resolves (`git rev-parse --verify --quiet <hex>^{commit}`) and is
     an ancestor of the commit that names it.
2. Any other commit that touched the same paths (a person's commit, a pull request's own committed
   output, a squash subject copied from the bot) counts as a record of at most itself. That is the
   conservative answer: a forged subject can only make the recorded state look older, never newer.
3. `git merge-base --is-ancestor` exits 1 for "no" and above 1 when git itself fails. Under
   `set -e`, capture the status, treat only 0 and 1 as answers, and fail on anything else. Otherwise
   "git broke" and "not an ancestor" read the same.

## Evidence

- sjawhar/legion#2493: `main_build_base` (`.github/scripts/release-push.sh:665`) finds the build
  main holds this way, and push-built lands nothing when that build is newer than its own.
  `is_ancestor` (`:483`) is rule 3.
- The `forged` case in `.github/scripts/release-push.test.sh` gives a person's commit the bot's
  exact subject. That commit is read as a build of itself, so push-built takes no decision from the
  subject.
- The reviewer's security pass at 359cbd66 confirmed rule 2: a forged subject can only move
  `main_build_base` older, because of the ancestor check.
