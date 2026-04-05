---
title: "Conventional commit semver bumping for envoy-plugin releases"
category: ci
tags:
  - release-workflow
  - semver
  - conventional-commits
  - envoy-plugin
  - github-actions
date: 2026-04-05
status: active
related_issues:
  - "#262"
---

# Conventional Commit Semver Bumping for Envoy-Plugin Releases

## Context

The repo has two parallel release workflows with nearly identical version-bumping logic:

- `release.yaml` — daemon + opencode-plugin, uses `v*` tags
- `release-envoy-and-plugin.yaml` — envoy + envoy-plugin, uses `legion-envoy-v*` tags

Both use a shell-based conventional commit classifier (no external tools).

## Pattern: Adding Semver Bumping to a New Release Workflow

When a new package needs its own release lifecycle:

1. **Copy the version bumper** from `release.yaml`'s `version` job
2. **Change the tag prefix** (e.g., `v*` → `legion-envoy-v*`)
3. **Change the package.json path** for the first-release fallback
4. **Add `should_release` gate** to all downstream jobs
5. **Add version commit-back** in the release job (uses `DEPLOY_KEY`, not `GITHUB_TOKEN`)

## Gotchas

### `should_release` requires `needs: [version]` on every gated job

The `docker` job originally only `needs: [envoy]`. Adding the `should_release` gate required
also adding `needs: [version]` so it could access `needs.version.outputs.should_release`.
Easy to miss when retrofitting an existing workflow.

### Version is written twice (intentional)

The bumped version is set in `package.json` in both the `plugin` job (before build/pack) and
the `release` job (before committing back to main). This is necessary because GitHub Actions
jobs run on fresh checkouts — the `plugin` job's filesystem changes don't persist to `release`.

### `fetch-depth: 0` is required

Both the `version` job and the `release` job need full git history:
- `version` needs `git tag -l` and `git log TAG..HEAD` to work
- `release` needs full history to push tags

The default `actions/checkout` does a shallow clone (`fetch-depth: 1`) which silently breaks
tag discovery and commit scanning.

### Tag format migration: strip old suffixes

When migrating from `legion-envoy-v0.13.4-f4d7c12` (version+SHA) to clean `legion-envoy-v0.14.0`,
the parser must handle both formats:

```bash
CURRENT="${LATEST_TAG#legion-envoy-v}"   # "0.13.4-f4d7c12" or "0.14.0"
CURRENT="${CURRENT%%-*}"                  # "0.13.4" or "0.14.0"
```

**Limitation:** `%%-*` strips from the first `-`, which would break on prerelease semver tags
like `1.0.0-beta.1`. This repo doesn't use prerelease tags in release workflows, but if that
changes, the stripping logic needs revision.

## When to Extract a Composite Action

Two copies of the version bumper are acceptable. If a third release workflow is added, extract
the logic into `.github/actions/semver-bump/action.yml` to avoid triple-duplication.
