---
title: "A test that proves a binary absent owns the whole PATH: the stub bin alone, with sh linked into it, never the stubs ahead of the runner's PATH"
category: testing
tags:
  - go
  - PATH
  - stub-binaries
  - negative-control
  - probe-image
  - github-runner
date: 2026-10-08
status: active
module: packages/daemon/cmd/legion
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1846"
---

# A test that proves a binary absent owns the whole PATH

Extends docs/solutions/testing/fake-cli-on-path-outputs-from-files-and-a-call-log.md.

- A fake first on PATH is right for observing a call; it is wrong for proving an absence. With the
  process's own PATH appended behind the stubs, removing a stub lets the host's real binary answer
  for it, and the negative case passes for the wrong reason — on the machine that happens to have
  the binary, which a worker pod does not and a GitHub runner does.
- Set `PATH` to the stub directory alone (`t.Setenv("PATH", roots.bin)`), and link into it the
  interpreter the code under test shells out with (`sh`, resolved from the process's PATH with
  `exec.LookPath` before the override), so `sh -c` still runs while nothing else leaks in.
- A negative test that passes in one environment and fails in another is the symptom: ask which
  binary the environment supplied behind the stubs before touching the assertion.

## Evidence

sjawhar/legion#1846's `cmd/legion/probe_image_test.go` stubbed the twelve binaries the capability
check looks for and prepended the stub bin to `os.Getenv("PATH")`. Its `python3 off PATH` case
passed in the worker pod (no python3 on the image) and failed on CI's runner, whose own `python3`
satisfied both the `toolchain` row's lookup and the fake omp's `command -v python3` (Tests run
37733156056 at ff33c8c9). The fix set `PATH` to the stub bin alone and symlinked the runner's `sh`
into it; the case then passed on both, and a `/tmp/fakepy/python3` prepended to the pod's PATH
reproduced the runner's failure before the fix and not after.
