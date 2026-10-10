---
title: "A branch that says a directory holds nothing clears what an earlier run left, and the proof rig reuses one state directory across outcomes"
category: testing
tags:
  - stale-state
  - state-directory
  - gh-config-dir
  - proof-rig
  - negative-control
  - controller-start
date: 2026-10-10
status: active
module: packages/daemon/cmd/legion
related_issues:
  - "LEGION-668"
  - "sjawhar/legion#1878"
---

# A branch that says a directory holds nothing clears what an earlier run left, and the proof rig reuses one state directory across outcomes

- A state directory outlives the run that wrote it. A branch whose stderr line says the
  directory holds nothing ("gh acts as nobody") removes what an earlier run left there;
  `os.MkdirAll` on an existing directory is a no-op and leaves the earlier run's credential for the
  next process to act with. Reset means remove the files, not create the directory.
- A proof rig whose every scenario starts from a fresh state directory structurally cannot find
  this. Add one scenario that runs two starts on the same state directory with different outcomes
  (first against a daemon that has the resource, then against one that does not) and asserts the
  second run's claim about the directory.

## Evidence

`legion controller start` against a daemon with no GitHub App printed `[legion] the daemon has no
GitHub App to act as; the controller's gh acts as nobody` and left `<state_dir>/gh/hosts.yml` from
a start against a daemon that had one; the stub Oh My Pi's `gh auth token` answered the stale
token. The tester's scenario F found it (`.legion/LEGION-668/test.json`, failures[0]; red test
`TestControllerStartWithNoGitHubAppRemovesAStaleHostsFile` at 06f586b74ecb); fixed in 533cc8f3149b
(`packages/daemon/cmd/legion/controller_credential.go`, the no-App branch removes `hosts.yml` and
`config.yml`). The implementer's rig (scenarios A, B, C) had `rm -rf "$work/state"` before every
scenario.
