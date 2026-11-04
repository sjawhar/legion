---
title: "Turn off an upstream gate with its unconditional switch, never with a value that matches the current version"
category: testing
tags:
  - oh-my-pi
  - profiles
  - pins
date: 2026-11-02
status: active
module: scripts/e2e
related_issues:
  - "sjawhar/legion#2163"
---

# Turn off an upstream gate with its unconditional switch, never with a value that matches the current version

Extends docs/solutions/testing/a-settings-proof-asserts-the-relation-between-two-reads-of-the-same-binary-never-a-literal-upstream-default-LEGION-578.md.

- A setting that silences a gate by matching an upstream constant (a setup version, a schema revision) stops working when the next pin moves the constant, and nothing fails:
  the gate opens again and whatever waits behind it hangs.

- When the upstream offers a switch that turns the behaviour off whatever the version, write that switch, and say in the comment that it holds across pins.

- For Oh My Pi's setup wizard in a profile a harness writes, that switch is `startup.setupWizard: false`.

## Evidence

The pinned Oh My Pi 18.13.3 opens its setup wizard in a fresh profile, and the devbox run's controller pane never started its session, so `controller-up` timed out.
Round 28 wrote `setupVersion: 2` into the three model-route libraries;
the build opens the wizard when `setupVersion` is below its `CURRENT_SETUP_VERSION`, 2 in that build, so a pin that adds a setup step would have stalled every proof using those libraries with no error (review 5732651189).
Round 29 (`67e3bbc6`) writes `startup.setupWizard: false` instead (`scripts/e2e/lib/install-key-file-model-route.sh:315-319`, `install-model-gateway.sh:385-389`, `install-box-login-route.sh:208-212`).
In a pty, the pinned build showed no `Setup step` with it and showed the wizard with it removed.
