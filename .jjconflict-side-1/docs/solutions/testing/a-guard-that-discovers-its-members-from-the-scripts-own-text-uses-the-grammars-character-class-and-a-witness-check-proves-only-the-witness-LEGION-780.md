---
title: "A guard that discovers its members from the script's own text uses the grammar's character class, and a witness check proves only the witness"
category: testing
tags:
  - live-proof
  - stage4b
  - prerequisites
  - jq
  - grep
  - vacuity
date: 2026-10-16
status: active
module: scripts/e2e/stage4b-sandbox-tree.sh
related_issues:
  - "LEGION-780"
  - "sjawhar/legion#1982"
---

# A guard that discovers its members from the script's own text uses the grammar's character class, and a witness check proves only the witness

Extends docs/solutions/testing/a-jq-module-reached-through-an-include-keeps-its-defs-flat-and-a-live-proof-compiles-every-module-on-the-hosts-jq-at-its-prerequisites-LEGION-578.md.

- A guard whose member list is read from the script itself (`included_jq_modules`: the jq modules
  the script `include`s, so a module joins the compile guard the day a checkpoint includes it)
  discovers members with the character class of what it scans accepts, not the convention the
  current names follow. jq takes any relative path as an include string, so the class is
  `[A-Za-z0-9_./-]`; `[a-z0-9-]` reads six kebab-case names and silently drops `with_underscore`,
  `Dotted.v2` and `nested/module`, and the guard then compiles nothing for them.
- When the function reads the file that defines it, make the class self-excluding and say so:
  `include "[A-Za-z0-9_./-]*"` does not match the function's own grep text (`[` follows the quote)
  or its sed text (`;` follows `//`), where `[^"]*` would list both as modules. Test the function
  over the real script and pin the list it must print.
- A non-empty check and a contains-the-witness check prove only that the witness survived; a class
  that drops every other member passes both. Pin discovery with names outside the convention
  (underscore, capital, dot, directory) beside the real script's list, and make the test's expected
  output the whole sorted list.
- Verify a module name reaches jq as data, not program: the class above excludes `"` and `\`, and
  `jq -n -L "$libdir" "include \"$module\"; true"` interpolates only what that class let through.

## Evidence

sjawhar/legion#1982 (LEGION-780) added `included_jq_modules` and `jq_modules_compile` to Stage 4b's
`prerequisites`. The tester's round-1 red test (`included_jq_modules > reads an include whatever
its name's characters`, 0214cf3fc004) fed the lifted function a script with the four names above
and received `stage4b-tools` alone; the two vacuity fails in `prerequisites` (a non-empty list, a
list naming `stage4b-tools`) both held over that list. The fix widened the class, the test passed
unmodified under jq 1.6, 1.7.1 and 1.8.2, and the real script's list stayed its six modules.
