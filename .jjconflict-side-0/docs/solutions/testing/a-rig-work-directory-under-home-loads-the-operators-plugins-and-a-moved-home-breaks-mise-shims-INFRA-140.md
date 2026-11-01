---
title: "A rig work directory under your home loads your own Oh My Pi plugins, and a moved HOME breaks every mise-shimmed tool a test child runs"
category: testing
tags:
  - rig
  - oh-my-pi
  - skill-scenarios
  - mise
  - isolation
date: 2026-10-24
status: active
module: pi-legion
related_issues:
  - "INFRA-140"
---

# A rig work directory under your home loads your own plugins, and a moved HOME breaks mise shims

Two isolation faults of `packages/pi-legion/scripts/skill-scenarios/rig.sh` gave wrong results with no error. Each cost more than half an hour to find.

## 1. Oh My Pi reads the nearest `.omp` above a run's working directory

A rig run gives its agent a HOME of its own and a profile with the label's packed plugins. The run's working directory is under `SKILL_SCENARIOS_WORK`. Oh My Pi also walks up from the working directory for a project `.omp`, past HOME. With `SKILL_SCENARIOS_WORK` under the operator's home, it found `$HOME/.omp` and loaded the operator's own plugins (`$HOME/.omp/plugins`) in place of the label's. The live read then answered `Unknown skill: asd-ste100` for a skill the label ships. A comparison of two labels would have compared the operator's plugins with themselves.

The run's Oh My Pi log shows it: the `extension instance loaded` lines name files under `$HOME/.omp/plugins`, and a `plugins:` warning names `"root":"<home>/.omp/plugins"`.

**Rule:** a rig work directory has no `.omp` in it or above it. `rig.sh` refuses one, naming the `.omp` it found (`omp_ancestor`). Read the `extensions loaded:` line of `rig.sh live-read` to check which plugins a label's runs load.

## 2. A test child with a moved HOME cannot run a mise shim

`rig.test.ts` runs rig functions in child processes with a HOME of the test's own. On a host where `jq` and `jj` on PATH are mise shims, mise under that HOME reads the operator's `~/.config/mise/config.toml` as untrusted and exits with an error. Every `jq` call then printed nothing. An empty open-ask count is not `0`, so the turn loop answered and continued for all 8 turns. Six tests failed this way under `env -i`, and passed in the operator's shell only because Bun, started through its own mise shim, had put mise's tool directories first on PATH. CI uses real binaries, not shims, so CI never failed.

**Rule:** a test that moves HOME for a child keeps the caller's `MISE_DATA_DIR` and `XDG_CONFIG_HOME`, as `scripts/e2e/lib/omp-home.sh`'s `make_omp_home` does. Run a package's tests once under `env -i HOME="$HOME" USER="$USER" PATH="$PATH"` before you trust a local pass.
