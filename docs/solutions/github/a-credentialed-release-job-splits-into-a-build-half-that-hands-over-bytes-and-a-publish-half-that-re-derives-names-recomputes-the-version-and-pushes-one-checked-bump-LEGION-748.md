---
title: "A credentialed release job splits into a build half that hands over bytes and a publish half that re-derives names, recomputes the version and pushes one checked bump; a checker pins the shape across every YAML level the credential can reach"
category: github
tags:
  - release-workflows
  - deploy-key
  - oidc
  - cache-mode
  - build-publish-split
  - release-push
  - push-main
  - tripwire
  - check-release-credentials
  - reusable-workflow
date: 2026-10-15
status: active
module: .github/workflows/release.yaml
related_issues:
  - "LEGION-748"
  - "sjawhar/legion#1969"
  - "LEGION-772"
  - "LEGION-773"
---

# A credentialed release job splits into a build half that hands over bytes and a publish half that re-derives names, recomputes the version and pushes one checked bump; a checker pins the shape across every YAML level the credential can reach

## The rule

1. **Split by what runs, not by what publishes.** The build half (`contents: read`, no secret)
   runs everything that is not this repository's code — install, pack, build, test, smoke —
   and ends with a tripwire step that prints what it can reach and fails on a credential. The
   publish half holds `contents: write`, `id-token: write`, `attestations: write` or the deploy
   key, sets `cache-mode: none` (GitHub's job key; `actions/setup-go` then prints
   `Cache restore skipped: the effective cache-mode 'none' does not permit reads.`), checks out
   with `persist-credentials: false`, and runs only `git`, `gh`, `jq`, `sha256sum`,
   `npm view`/`npm publish <tarball>`, `docker buildx imagetools` and the repository's own
   release script. A credential narrowed to a step inside one job is not a split: an earlier
   step's dependency code can plant a `git` on `$GITHUB_PATH` for the later step.
2. **Hand over bytes and one scalar; re-derive everything else.** The build half uploads an
   artifact and outputs a version. The publish half names every file itself from its own
   checkout (the targets list, the manifest's `name`), refuses the artifact unless it holds
   exactly those regular files (`release-push.sh handed`), writes `SHA256SUMS` itself after that
   check, and, where two runs could take one tag, recomputes the version inside its concurrency
   group and refuses on a mismatch (`release-push.sh version --expect`). Decide in the spec what
   a mismatch means when the work is already done: this issue's `--expect` turns a benign race
   (the version already released by an earlier run) into a red job that needs a whole-run
   re-run, recorded on LEGION-772.
3. **A push with the deploy key pushes exactly one checked commit.** `git push <url>
   HEAD:refs/heads/main` is a write path from whatever HEAD is — on a `workflow_dispatch` from
   branch X, X — and the trigger is no guard; the base's `git push origin main` refused there
   only by accident (`src refspec main does not match any`). Before every push, after fetching
   `main`: HEAD has one parent and is one commit over the fetched tip (a bump on an older main
   commit is rebased, then checked again; a merge or root commit is refused), its tree differs
   from its parent's in the manifest alone, and the manifest is the parent's copy with
   `.version` set to the released version and nothing else (`jq -S`, so sorted-JSON equality: a
   BOM, duplicate keys, key order and the blob mode are not compared — say so). Check against
   the commit's own parent, before any push, rebase or "main already holds this version" exit,
   so a doctored commit is refused rather than dropped. Every refusal goes through the one sink
   (`fail_outstanding`: the `::error::` and the job-summary sentence that the publish and tag
   already succeeded). The commit the script could construct itself on the fetched tip would
   remove the five checks and the rebase (LEGION-772).
4. **Pin the shape with a checker that resolves YAML the way GitHub does.** Every property a
   credential or an interpreter can reach has three levels — the step, the job's `defaults`/
   `env`/`permissions`, the workflow's — and a job with no `permissions:` under a workflow with
   none runs under the repository's default grant, which the checker cannot read: refuse it. A
   tripwire pinned by its text is not pinned: refuse `if:` and `continue-on-error:` on the step,
   `continue-on-error:` on the job (a failed job reports `success` to `needs`), and an effective
   shell that is not bash/sh (`defaults: run: shell: 'cat {0}'` prints the body). Name the
   secret in every spelling GitHub resolves (`secrets.DEPLOY_KEY`, `secrets['DEPLOY_KEY']`, any
   case) and refuse any other `secrets.*` or `toJSON(secrets)` in a build job. Write the threat
   model in the header: the checker holds the shape against a well-meant later edit, not against
   someone editing the workflow to hide a command, who could remove the checker from CI; the
   pass-through class (a variable in command position, `xargs`, `nohup`, an unlisted program)
   is accepted on that ground.
5. **A job that calls a reusable workflow is outside the checker.** The checker reads jobs with
   `steps:`; a `uses:` job is checked, if at all, by the called workflow's own file. Before
   collapsing token-identical publish jobs into a reusable workflow (LEGION-772), teach the
   checker to follow `uses:` into the called file and to refuse `secrets: inherit`, or the
   dedupe moves those jobs out of coverage; today a comment alone keeps `secrets: inherit` off
   `release.yaml`'s `worker_image`, which builds a pull-request-authored Dockerfile.
6. **The pull request's commit types are release inputs.** With squash merges the title becomes
   `main`'s subject and the commit subjects its body, and `release-bump.sh` reads both; a
   `feat`/`fix` commit touching a release path (a README under `packages/pi-envoy/`, a comment
   in `packages/daemon/docker/worker.Dockerfile`) cuts that artifact's release over a
   comment-only change. Type CI and documentation commits `ci`/`docs`, and retype before the
   pull request opens (a rewrite after the push needs the shared-repository guard in
   `skill://legion-worker/references/conflicts-and-rewrites.md`).

## Evidence

- sjawhar/legion#1969: `release.yaml`'s `legion`, `envoy_plugin`, `pi_envoy`, `pi_legion` and
  `release-envoy-listener.yaml`'s `release` became `*_build`/`*_publish` pairs;
  `release-envoy-infra.yaml`, which already had the shape, adopted the shared version command,
  the tripwire and `cache-mode: none`. `check-release-credentials.sh` reports
  `7 credentialed job(s) and 8 build job(s)` over the three files.
- Rule 1's facts were measured on sjawhar/legion-smoke runs 38758454703 and 38758671814
  before any workflow was edited: `cache-mode: none` accepted, restore and save skipped under
  it, the control job without it restoring (`Cache hit for: setup-go-…`), a clean ubuntu-24.04
  runner's credential baseline (the tripwire's `git config --get-regexp` empty, `~/.ssh` absent,
  the OIDC variables unset), `docker buildx imagetools inspect` without
  `docker/setup-buildx-action`.
- Rule 3's three review rounds: round 1 found the write path (a branch two commits ahead landed
  on main; the base pushed nothing); round 2 found the one-commit rule narrowed (a manifest-only
  commit carrying `scripts.postinstall` with the version already set, so `tag` committed
  nothing; a merge commit whose first parent was an older main commit and whose tree was the old
  tree plus the bump — count 1, path diff the manifest, `main` fast-forwarded back to the old
  tree); round 3 found the checks ran only when the parent was main's tip, so the
  superseded-version exit could drop a doctored commit unchecked. `release-push.test.sh` cases
  2 to 2g pin each shape against a bare origin with a recording pre-receive hook: no push seen,
  origin's tip and manifest untouched, the planted value never printed.
- Rule 4's rounds: the tester's red cases (npm subcommands, interpreters over a `scripts/`
  program, `shell: python`, `secrets['DEPLOY_KEY']`, a build job with no `permissions:`
  anywhere); review round 1 (workflow-level `env:` and `defaults:` unread); round 2 (job-level
  `continue-on-error`, `defaults: run: shell: 'cat {0}'`). The checker's fixture harness holds
  69 cases at the approved head.
- Rule 6: the pull request's workflow commits were retyped from `feat(ci)` to `ci(release)`
  before it opened, since the differential of `release-push.sh version` showed the plugins'
  README comments and the Dockerfile comment inside their release paths; the merge-triggered
  listener run cuts `legion-envoy-v13.4.1` for a `fix(dispatch)` already on main, not for this
  change.
