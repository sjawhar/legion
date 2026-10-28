---
title: "A shared bash helper sets no EXIT trap"
category: best-practices
tags:
  - shell
  - trap
  - cleanup
  - process-substitution
date: 2026-10-28
status: active
module: .github/scripts
related_issues:
  - "INFRA-629"
  - "sjawhar/legion#2419"
---

# A shared bash helper sets no EXIT trap

## The rule

1. A function that more than one command calls never runs `trap … EXIT`. Bash holds one EXIT trap
   per shell, so the helper's trap silently replaces the caller's, such as the one that removes a
   key directory.
2. Keep the helper's state out of files: read a listing through process substitution
   (`while … done < <(find "$dir" -mindepth 1 -print0 | sort -z)`), then check its status with
   `wait "$!" || fail …`, so a failed `find` still fails the command.
3. When a temporary file cannot be avoided, remove it on every path inside the helper, and let
   only the command that owns the script's lifetime set the EXIT trap.

## Evidence

- Review round 1 of sjawhar/legion#2419: `check_hand_over`, taken out of `release-push.sh handed`
  so `push-fold` could share it, set `trap 'rm -f "$listing"' EXIT`. That replaces
  `deploy_key_env`'s trap that removes the deploy key's directory; it was safe only because
  `push-fold` happened to call the helper before `land_on_main`. Fixed in 6b255451bb62 with
  process substitution and `wait "$!"`, `handed`'s messages and tests unchanged.
