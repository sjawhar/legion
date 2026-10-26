---
title: "A shared test helper keeps its contract, a new marker shape gets its own wrapper, and the callers a pod skips run under unshare"
category: testing
tags:
  - test-helpers
  - go-test
  - pid-namespace
  - unshare
  - worker-pod
  - skipped-tests
date: 2026-10-25
status: active
module: packages/daemon/internal/launcher
related_issues:
  - "INFRA-271"
  - "sjawhar/legion#2179"
---

# A shared test helper keeps its contract, a new marker shape gets its own wrapper, and the callers a pod skips run under unshare

Extends docs/solutions/testing/a-struct-that-gains-a-slice-field-stops-being-comparable-grep-every-equality-over-it-the-suites-that-panic-are-the-ones-a-pod-skips-LEGION-578.md.

- **When a shared helper must read a new shape, leave the helper as it is and add a narrower
  wrapper over it.** If you change what an existing helper returns, every caller still compiles
  and now reads something else. Callers whose assertion compares against a value then read the
  zero value. They fail when the comparison is `!= "gone"`, and they pass vacuously when it is
  `== "survived"`.
- **A helper that parses a fixture fails the test on a shape it does not know.** Returning zero
  values for an unexpected shape lets an assertion read nothing.
- **Run the callers your environment skips before you push.** The launcher's PID-namespace tests
  (`namespace_test.go`) need `sudo -n unshare --pid …`. They call `t.Skipf` without it and
  `t.Fatalf` under `GITHUB_ACTIONS`, so a pod run is green and CI is their first run. In a pod,
  put a `sudo` shim first on `PATH`. The shim drops `-n` and the `--setuid`/`--setgid` pairs,
  then runs `unshare -r` with the rest. Run
  `PATH=<shim dir>:$PATH go test -run TestPIDOneLauncher ./internal/launcher/`. Then disable the
  PID-1 cleanup by mutation once, and see the tests fail, so you know they assert.

## Evidence

- The branch changed `published(t, marker)` to split three fields
  (`<pid> <socket> <credential>`) for a new stand-in child. The PID-namespace scripts write
  `<pid> <result>` (`printf '1 gone\n'`), so their result landed in the socket field and read
  back empty.
- The implementer's pod run was green because those tests skipped. CI run 40168372424 failed
  with `detached process 9  into the next generation after stop` (an empty result between the
  two spaces).
- Fix `1312d9e2fb1f`: `published` returns the pid and the rest unchanged again, and
  `publishedAll` splits the three fields on top of it.
- The tester then ran all three PID-namespace tests as PID 1 through the shim and saw them pass.
  With the cleanup disabled, they failed with `survived into the next generation` and
  `left an adopted orphan: zombie`.
