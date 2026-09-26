---
title: "A repository's git config serves one caller; a second caller that jj runs git for overrides it through GIT_CONFIG_COUNT/KEY_n/VALUE_n pairs — and git 2.44's credential.interactive is why the pane's config broke provisioning only in the worker image"
category: daemon
tags:
  - git-credential
  - credential-interactive
  - git-config-env
  - jj
  - workspace-provisioning
  - kubernetes-runtime
  - git-version
date: 2026-09-15
status: active
module: packages/workspace, packages/daemon/src/cli/workspace-init.ts
related_issues:
  - "LEGION-178"
  - "LEGION-177"
  - "sjawhar/legion#1121"
symptoms:
  - "a Kubernetes pod's workspace-init container dies on `jj git fetch` with `fatal: unable to get password from user` after `LEGION_GRANT_FILE is missing (and LEGION_GRANT is unset)`, but only on the second and later pods for a repository (a resurrected root, a respawned worker, a sibling worker)"
  - "the same provisioning works on the tmux daemon host, whose git is older"
---

# A repository's git config serves one caller; a second caller overrides it through `GIT_CONFIG_*` pairs

## What broke

`provisionIssueWorkspace` (`packages/workspace/src/workspace.ts`) ends every provisioning by
writing the **pane's** git configuration into the shared clone: `credential.helper` and
`credential.https://github.com.helper` both name the pane credential helper (`legion credential`),
and `credential.interactive=false` keeps a pane's git from ever prompting. That is correct for the
pane — its `jj git push` redeems the pane's grant through that helper.

The **next** provisioning of the same clone is a different caller. Its `jj git clone` / `jj git
fetch` run under `createProvisioningCredential`'s environment: an askpass script that answers
`x-access-token` and the repository installation token, plus `GIT_TERMINAL_PROMPT=0`. No grant
exists there (the daemon host, or a pod's init container), so git's first stop — the configured
pane helper — fails with `LEGION_GRANT_FILE is missing`. On git 2.43 that is harmless: the setting
`credential.interactive` does not exist yet, git ignores it, and the askpass fallback answers. On
git 2.44 and newer the same clone config means *never ask a user program*, so after the helper
fails git dies: `fatal: unable to get password from user`. The worker image ships git 2.47.3; the
dogfood tmux box ships 2.43.0; the CI runner 2.55.0. The tmux deployment never saw the bug, and
the first pod for a repository never saw it either (a fresh clone has no config yet). Every
resurrection and every respawn on Kubernetes did (LEGION-177's run: three launch failures, tree
`launch-failed`).

## The rule

**The persisted repository config belongs to the pane. Provisioning does not rewrite it; it
overrides it for its own commands, in its own environment.** The controller's ruling on
LEGION-178 rejected both alternatives — not writing `credential.interactive=false` (loses the
pane's no-prompt guarantee) and a run-local `git config --unset` (mutates the shared clone per
run) — for exactly this reason: a config file has one owner, and a second consumer with different
needs reaches git through the environment of the command it runs.

The override is seven environment pairs in `createProvisioningCredential`:

```
GIT_CONFIG_COUNT=3
GIT_CONFIG_KEY_0=credential.helper                     GIT_CONFIG_VALUE_0=     # empty: resets the list
GIT_CONFIG_KEY_1=credential.https://github.com.helper  GIT_CONFIG_VALUE_1=!'<the one-shot helper>'
GIT_CONFIG_KEY_2=core.hooksPath                        GIT_CONFIG_VALUE_2=/dev/null  # no hook the clone carries runs with the token
```

beside an empty `GIT_ASKPASS` (git reads it as no askpass program at all, not `core.askPass`
either), `GIT_TERMINAL_PROMPT=0`, and `GIT_ALLOW_PROTOCOL=https`, which refuses every transport
but the one github.com needs (so no rewrite reaches a program through a local path's
`uploadpack`, `core.sshCommand` or `ext::`). The clone and the fetch also pass
`--config=git.executable-path=git`, so a repository's `git.executable-path` never runs. The one-shot helper answers `get` with the installation
token; git asks it for https://github.com alone, so a remote that a URL rewrite in the clone's
config sends to another scheme, host or port is asked for nothing. That rewrite is the reason the
helper is scoped: every agent of a tree writes the shared clone's config. The fix LEGION-178
first shipped kept an askpass that answered every prompt, and re-enabled it with
`credential.interactive=true`; that askpass handed the token to whatever host a rewrite named.

Three git facts make the helper reset work:

- `GIT_CONFIG_*` pairs are **command scope**, read after the system, global, and repository
  files, so they win.
- An **empty `credential.helper` value clears every helper read so far** — including the
  URL-specific `credential.https://github.com.helper`, because git's urlmatch delivers both keys
  to the same callback in read order. The regression test proves this against a real clone that
  carries both keys (`workspace.test.ts`, "gets the one-shot credential for github.com alone,
  without running the pane helper").
- A credential helper is not a prompt, so the clone's persisted `credential.interactive=false`
  does not stop it. Nothing needs re-enabling.

Why **pairs, not `-c`**: jj, not our code, spawns the git that fetches. `jj git fetch -R <clone>`
accepts no git flags, but jj's own config reader (gitoxide) honours `GIT_CONFIG_COUNT` and the
subprocess inherits the environment — verified on jj 0.45.1 with an interposed `git` that
recorded every variable of the provisioning environment untouched (eight names then; the set has
grown since, by the same route). The same env reaches the pod through `legion
workspace-init`'s `processEnvRunner` and the daemon host through `createDaemonRunner`; both lay it
over the process environment with the caller's env winning.

The mirror image already existed: every **pane** carries `GIT_CONFIG_COUNT: "0"`
(`processes.ts`, the root and worker pane environments), so a pane's git ignores any `GIT_CONFIG_*`
it might inherit. Two callers, two environments, one config file.

## What to do next time

- Before writing a `credential.*` (or any behaviour-changing) key into a repository that more
  than one process uses, name every caller that will run git there and ask which of them the
  file is for. The others get an environment override on their own commands.
- The token stays out of `GIT_CONFIG_VALUE_n` — it travels only through the one-shot helper's
  `LEGION_PROVISIONING_TOKEN`. `isSecretLikeName` (`environment.ts`) matches `_KEY` only at the
  end of a name, so `GIT_CONFIG_KEY_0` is not scrubbed anywhere; a secret placed in a
  `GIT_CONFIG_VALUE_n` would ride any environment that copied these pairs.
- A git behaviour that differs by version is invisible on a green local run: check
  `git --version` on every surface the code runs on — dev box, CI runner, worker image — before
  trusting a local pass.
- `jj workspace update-stale` and the settings read/write run without the credential env — only
  the clone and the fetch talk to the remote. Keep it that way; the env is a credential.

## Related

- `../testing/a-test-that-reads-repository-git-config-must-pin-its-global-and-system-scopes.md`
  — the review finding this fix's own test hit.
- `../testing/isolate-git-from-the-pane-credential-helper-for-identity-proofs.md` — the same
  helper, the other direction: a pane's git silently redeeming the pane's grant for a proof that
  meant to use another identity.
- `../github/a-worker-image-path-filter-covers-the-code-the-init-container-runs.md` — why this
  PR's own head had to be built as an image before the kind proof.
- `packages/daemon/src/daemon/AGENTS.md`, "Workspace clone" bullet — the standing rule.
