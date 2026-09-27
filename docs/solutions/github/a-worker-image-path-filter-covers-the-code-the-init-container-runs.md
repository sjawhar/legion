---
title: "A worker-image build's pull_request path filter must cover the code the container executes, not only the Dockerfile — or a pod-behaviour change is proven on an image without it"
category: github
tags:
  - github-actions
  - path-filters
  - worker-image
  - kubernetes-runtime
  - production-like-proof
date: 2026-09-15
status: active
module: .github/workflows/worker-image.yaml
related_issues:
  - "LEGION-178"
  - "sjawhar/legion#1121"
  - "sjawhar/legion#966"
symptoms:
  - "a PR that changes what a pod's init container or entrypoint does has no `Worker Image / build` check, so no `sha-<head>` image exists to run its kind proof on"
  - "a kind proof of a pod-side fix passes or fails on an image built from `main`, not from the PR"
---

# A worker-image path filter must cover the code the container executes

## The gap

`worker-image.yaml`'s `pull_request.paths` named the Dockerfile directory, the OMP pin, and the
workflow file — the *image files*. LEGION-178's fix changed none of them: it changed
`packages/workspace/src/workspace.ts`, which `legion workspace-init` executes inside every pod's
init container. Without a trigger change, the PR's head would have had no image build, and the
kind proof (acceptance 1 and 2: a resurrected root's second-generation pod provisions on the
existing clone) would have run on the latest `main` image — an image **without the fix**, proving
either nothing or the wrong thing. The image is built only in CI (`docs/kubernetes.md`: never
`docker build` on a workstation), so there is no local escape hatch.

## The rule

The question for the path filter is not "does this file go into the image?" but **"does the
container run this code?"** The `legion` CLI compiled into the image is built from the whole
daemon checkout, so any source it executes at pod runtime is image behaviour. For the init
container that is `packages/daemon/src/cli/workspace-init.ts` and everything under
`packages/workspace/**`.

A hand-kept list of the code the container runs misses some: one covered the plugin
(`packages/pi-envoy/**`) but not `packages/contracts/**` or `packages/envoy-client/**`, which the
plugin bundles, and named three files of the daemon the CLI is compiled from. So the filter names
every context source `worker.Dockerfile` copies, whole (`packages/daemon/**`, not chosen files
under it), and `.github/scripts/check-image-trigger-paths.sh` parses the Dockerfile and fails the
lint job when the filter misses a file the build reads. Building on every daemon pull request
costs little. Replaying the last 50 merged pull requests (as of 2026-09-27) through the filter
before and after it named the daemon whole, with git's `:(glob)` pathspecs:

```bash
paths() { git show "$1:.github/workflows/worker-image.yaml" | python3 -c 'import sys,yaml; print(" ".join(":(glob)"+p for p in yaml.safe_load(sys.stdin)[True]["pull_request"]["paths"]))'; }
old=$(paths dab7f13d); new=$(paths 2b6dec37)
gh pr list -R sjawhar/legion --state merged -L 50 --json number,mergeCommit -q '.[] | "\(.number) \(.mergeCommit.oid)"' |
  while read -r n sha; do
    git diff --quiet "$sha~1" "$sha" -- $old; o=$?; git diff --quiet "$sha~1" "$sha" -- $new; w=$?
    echo "$n old=$o new=$w"
  done | awk '{o+=($2=="old=1"); w+=($3=="new=1")} END {print "merged PRs:", NR, "built before:", o, "built after:", w}'
# merged PRs: 50 built before: 27 built after: 28   (the one added is #1466, packages/contracts)

gh api "repos/sjawhar/legion/actions/workflows/worker-image.yaml/runs?event=pull_request&status=success&per_page=100" \
  --jq '[.workflow_runs[] | ((.updated_at|fromdate)-(.run_started_at|fromdate))] | sort | "runs: \(length) median_s: \(.[length/2|floor])"'
# runs: 100 median_s: 274
```

Every widening of `pull_request.paths` applies to the PR that makes it: GitHub runs the workflow
file from the PR merge ref for `pull_request` events, so LEGION-178's own first push built its
head (`Worker Image` run 34938845664 → the digest its kind proofs ran on), and the trigger did
not need to land on `main` first.

## Where the rule lives

- `.github/workflows/worker-image.yaml` — the comment above `pull_request` says what the paths
  cover.
- `docs/kubernetes.md`, "How it is built — and the iteration rule" — the same list in prose.
- `.github/scripts/check-image-trigger-paths.sh` — fails the build when the list misses a file
  the Dockerfile reads.

## Related

- `pull-request-trigger-paths-follow-the-pr-head.md` — the earlier half of this rule: which
  *event* (`pull_request`, not `push`) so the check stays attached to every head. This learning
  is the *which paths* half.
