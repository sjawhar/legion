---
module: envoy
date: 2026-08-30
problem_type: build_error
component: tooling
severity: critical
symptoms:
  - "npm install/bun add of @sjawhar/opencode-legion-envoy fails because the published manifest depends on '@legion/contracts': 'workspace:*' and '@legion/envoy-client': 'workspace:*'"
  - "Every published version through 0.6.0 is uninstallable from the npm registry even though the release workflow ran successfully"
  - "opencode.json/tui.json referencing @sjawhar/opencode-legion-envoy@latest cannot install the plugin for any consumer outside the monorepo"
  - "bun pm pack produces a tarball with workspace:* correctly rewritten, but the artifact actually published to npm still contains the unrewritten workspace:* protocol"
root_cause: config_error
resolution_type: code_fix
related_components:
  - release-envoy-plugin.yaml
  - opencode-legion-envoy
  - pi-legion-envoy
  - legion/contracts
  - legion/envoy-client
tags:
  - npm-publish
  - bun-workspaces
  - workspace-protocol
  - monorepo-packaging
  - release-pipeline
  - devdependencies-bundling
---

# npm install of @sjawhar/opencode-legion-envoy failed for weeks: bun pm pack tarball built correctly, then npm publish silently republished the broken directory

## Problem

`@sjawhar/opencode-legion-envoy` was uninstallable from the public npm registry — every published version through `0.6.0` shipped

```json
"dependencies": {
  "@legion/contracts": "workspace:*",
  "@legion/envoy-client": "workspace:*"
}
```

`@legion/contracts` and `@legion/envoy-client` are internal bun workspace packages that have never been published to npm and never will be. Any consumer resolving `opencode.json`/`tui.json`, both of which pin `@sjawhar/opencode-legion-envoy@latest`, got a hard install failure — the plugin could not be installed at all, in any project, by any consumer, from any version published since the envoy-client extraction.

This was two independent bugs stacked on top of each other, and either one alone would have kept the package broken:

1. **The release workflow packed correctly and then discarded the correct artifact.** `.github/workflows/release-envoy-plugin.yaml` ran `bun pm pack` (which, in bun 1.3.14, does rewrite `workspace:*` to a concrete resolvable version inside the tarball it produces), then ran bare `npm publish` with no tarball argument. npm's `publish` with no path argument does not publish the tarball sitting in the working directory — it re-packs the **directory** itself using npm's own packing logic. npm does not understand the `workspace:` protocol, so npm's re-pack emitted the manifest verbatim, `workspace:*` and all. Bun's correctly rewritten tarball was built, sat on disk, and was never touched again.
2. **Even a correct rewrite could not have fixed installs on its own.** `@legion/contracts` and `@legion/envoy-client` are not published anywhere. Rewriting `workspace:*` to, say, `0.1.0` would just change the failure from "invalid version range" to "package not found" — `npm install` still fails because there is no `@legion/contracts` on the registry at any version.

## Symptoms

- `npm install @sjawhar/opencode-legion-envoy` (or any resolver processing `opencode.json`/`tui.json`'s `@latest` pin) fails with an unresolvable version range on `@legion/contracts` and `@legion/envoy-client`.
- The failure reproduces identically across every published version up through `0.6.0` — this was not a one-off bad release, it was structural to the release pipeline.
- Inspecting the published tarball on npm (`npm pack @sjawhar/opencode-legion-envoy` then reading `package.json` inside it) shows `workspace:*` deps verbatim, even though the CI logs show `bun pm pack` ran without error.
- Locally, running `bun pm pack` by hand in `packages/envoy-plugin` produces a tarball with the workspace deps correctly rewritten — the discrepancy between "works when I do it by hand" and "broken on npm" was the tell that the workflow's *publish* step, not the *pack* step, was the fault line.

## What Didn't Work

**Assuming `bun pm pack` alone was sufficient.** The initial framing of the bug was "the workspace-protocol rewrite isn't happening" — i.e., a version-rewrite problem to be fixed by getting bun to emit resolvable version numbers instead of `workspace:*`. That framing is half wrong and, even where right, insufficient:

- It's half wrong because `bun pm pack` *was already doing the rewrite correctly*. The tarball bun built in the `Pack plugin` / `Pack extension` step was never the broken artifact. The bug was entirely in the next step: `npm publish` with no arguments re-packs the current directory instead of using the tarball that already existed on disk. Two packers, two different understandings of `workspace:*`, and the wrong one won because it ran last and looked like a no-op ("just publish what's here").
- It's insufficient on its own because even a perfectly rewritten manifest — say `@legion/contracts` pinned to `0.1.0` — cannot install from npm, because `@legion/contracts` and `@legion/envoy-client` are not, and were never intended to be, published packages. A version-rewrite fix alone trades one install failure (`invalid version workspace:*`) for another (`404 not found`). Verifying "does the tarball's manifest look right" is not the same as verifying "does `npm install` of this tarball succeed outside the monorepo" — the former was checked and passed; the latter was never checked and would have failed.

The actual fix therefore could not be "make npm publish the bun-packed tarball" alone (bug 1) or "rewrite workspace deps to real versions" alone (bug 2) — it required removing the runtime dependency on the unpublished packages entirely.

## Solution

The fix applies identically to both released packages: `@sjawhar/opencode-legion-envoy` (`packages/envoy-plugin`) and the newly-introduced `@sjawhar/pi-legion-envoy` (`packages/pi-envoy`, the Oh My Pi extension sibling).

### 1. Publish the tarball explicitly, never the directory

Both release workflows changed from `bun pm pack && npm publish` (directory re-pack) to publishing the exact `.tgz` bun produced:

```yaml
- name: Pack plugin
  run: rm -f ./*.tgz && bun pm pack
  working-directory: packages/envoy-plugin

- name: Publish to npm
  run: |
    if npm view "@sjawhar/opencode-legion-envoy@${{ steps.version.outputs.version }}" version >/dev/null 2>&1; then
      echo "npm already has ${{ steps.version.outputs.version }}; skipping publish"
    else
      npm publish ./*.tgz --access public --provenance
    fi
  working-directory: packages/envoy-plugin
```

`rm -f ./*.tgz` before packing keeps the glob single-valued (a stale `.tgz` from a previous run would make `./*.tgz` expand to multiple arguments and break `npm publish`). `packages/pi-envoy` got the identical `Pack extension` / `Publish to npm` pair.

### 2. Bundle the shared workspace packages instead of publishing them

Rather than trying to publish `@legion/contracts` and `@legion/envoy-client` to npm (which would create two more packages to version and support), both consumers bundle those dependencies into a single self-contained JS file at pack time and stop depending on them as installable packages.

`packages/envoy-plugin/package.json`:

```json
{
  "main": "dist/src/server.js",
  "exports": {
    ".": { "import": "./dist/src/server.js" },
    "./server": { "import": "./dist/src/server.js" }
  },
  "files": ["dist", "src", "!src/**/__tests__", "skills"],
  "scripts": {
    "build": "bun build src/server.ts --root . --outdir dist --target bun --format esm --external '@opencode-ai/*'",
    "prepack": "bun run build && rm -rf skills && cp -r ../../skills skills",
    "postpack": "rm -rf skills"
  },
  "dependencies": {
    "@opencode-ai/plugin": "~1.14.46"
  },
  "devDependencies": {
    "@legion/contracts": "workspace:*",
    "@legion/envoy-client": "workspace:*"
  }
}
```

The two internal packages moved from `dependencies` to `devDependencies`. `main`/`exports` point at the bundled `dist/` output instead of `src/`. `prepack` runs `bun run build`, which invokes `bun build` targeting bun with esm output, bundling `@legion/contracts` and `@legion/envoy-client` straight into `dist/src/server.js` (`--root .` keeps the `src/` segment in the output path for a single entry point, so `main`/`exports` stay valid), then stages the repo `skills/` tree into the package for the tarball (`postpack` removes it again). Only `@opencode-ai/*` stays external (it's a real published peer/consumer dependency, not a workspace-only package). `bun pm pack` runs the standard npm/bun lifecycle (`prepack` then pack then `postpack`), which was empirically confirmed to fire correctly on bun 1.3.14 — the bundle exists in the tarball before packing happens.

`packages/pi-envoy` (the Oh My Pi extension) has the same shape plus one extra wrinkle: it also has to ship a non-JS asset (the `skills/` tree) and it has two extensions, only one of which (`envoy.ts`) is meant for external consumers — `legion.ts` is daemon-only infrastructure that must never load in a global session.

`packages/pi-envoy/scripts/prepack.sh`:

```bash
#!/bin/bash
# Tarballs ship only dist/: the release workflow rewrites omp.extensions to
# the bundle before packing and restores the committed manifest afterwards
# (see .github/workflows/release-pi-envoy.yaml). Packing with the committed
# source manifest would publish a package whose extension files are absent
# from the tarball, so fail fast instead.
set -euo pipefail
cd "$(dirname "$0")/.."
if ! jq -e '.omp.extensions == ["dist/envoy.js"]' package.json >/dev/null; then
  echo "pi-envoy: refusing to pack with omp.extensions=$(jq -c '.omp.extensions' package.json); rewrite it to [\"dist/envoy.js\"] first" >&2
  exit 1
fi
# Only the envoy extension ships: the legion extension is daemon
# infrastructure, loaded from repo checkouts.
bun build extensions/envoy.ts --outdir dist --target bun --format esm --external @oh-my-pi/pi-coding-agent
rm -f dist/legion.js
rm -rf dist/skills
cp -r ../../skills dist/skills
```

The committed `package.json` keeps `"omp": { "extensions": ["extensions/envoy.ts", "extensions/legion.ts"] }` so repo checkouts (the Legion daemon, local dev sessions) load TypeScript sources directly with no build step. The release workflow rewrites that field to the packed layout *only for the tarball*, then restores the original right after publish:

```yaml
- name: Point extensions at the packed bundle
  run: |
    cp packages/pi-envoy/package.json "$RUNNER_TEMP/pi-envoy-manifest.json"
    jq '.omp.extensions = ["dist/envoy.js"]' \
      packages/pi-envoy/package.json > tmp.json \
      && mv tmp.json packages/pi-envoy/package.json

- name: Pack extension
  run: rm -f ./*.tgz && bun pm pack
  working-directory: packages/pi-envoy

- name: Publish to npm
  run: |
    if npm view "@sjawhar/pi-legion-envoy@${{ steps.version.outputs.version }}" version >/dev/null 2>&1; then
      echo "npm already has ${{ steps.version.outputs.version }}; skipping publish"
    else
      npm publish ./*.tgz --access public --provenance
    fi
  working-directory: packages/pi-envoy

- name: Restore committed manifest
  if: always() && steps.version.outputs.skip == 'false'
  run: |
    if [ -f "$RUNNER_TEMP/pi-envoy-manifest.json" ]; then
      mv "$RUNNER_TEMP/pi-envoy-manifest.json" packages/pi-envoy/package.json
    fi
```

`prepack.sh` is the guard that makes this safe: it refuses to build/pack unless `omp.extensions` is already `["dist/envoy.js"]`, so a pack run outside the workflow's rewrite step (e.g. a maintainer running `bun pm pack` locally with the committed source-pointing manifest) fails loudly instead of silently shipping a tarball whose manifest points at `extensions/envoy.ts`/`extensions/legion.ts` — files that were never included (`"files": ["dist"]`).

### 3. Make runtime path resolution candidate-based

Bundling moves the running code from `src/`/`extensions/` into `dist/`, which breaks any `import.meta`-relative path that assumed the source layout. Path resolution to a sibling artifact became a small list of candidates checked with `existsSync`, covering both the packed (`dist/`) and repo-source layouts, throwing loudly if neither exists. `packages/pi-envoy/extensions/envoy.ts` resolves its bundled skills this way (`resolveSkillsDirectory`): `<module-dir>/skills` (packed — staged there by `prepack.sh`) then `<module-dir>/../../../skills` (repo source layout), throw if neither is found.

### Verification

- Tarballs from both packages were installed into bare `/tmp` directories (outside the monorepo, so bun/npm workspace linking could not paper over anything) via both `bun add` and `npm install`; imports resolved.
- The OMP extension tarball was loaded in a live session (`omp -e package/dist/envoy.js`) with its tools and packed `skills/` tree working end to end.
- An independent QA agent reproduced the full install-and-run verification separately.

## Why This Works

- **npm and bun disagree about `workspace:*`, and whichever tool re-packs last wins.** `bun pm pack` understands the bun-workspaces `workspace:*` protocol and rewrites it to a concrete version in the tarball it emits. `npm publish` given no path argument does not read an existing tarball off disk — it invokes its own packing logic against the current working directory, and npm's packer has no concept of `workspace:` at all, so it copies the field through unchanged. Running both tools in sequence without pointing `npm publish` at bun's tarball means the *last* packer's (mis)understanding of the protocol is what actually ships.
- **npm never installs a dependency's `devDependencies`.** This is what makes "move `@legion/*` to devDependencies" a real fix rather than a cosmetic one: a package's own `devDependencies` are only installed when you run `npm install`/`bun install` *inside that package's own directory* (i.e., for its own development/build), never when another project installs it as a dependency. bun workspaces still link `devDependencies` locally for development (so `bun run build` inside the monorepo still resolves `@legion/contracts` via the workspace symlink), but an external `npm install @sjawhar/opencode-legion-envoy` only ever looks at `dependencies`, and `@legion/*` no longer appears there — because the actual runtime code has already been bundled into `dist/` at pack time, there is nothing left at install time that needs to resolve those packages at all.
- **A pack-time guard closes the gap between the committed manifest and the publishable manifest.** `packages/pi-envoy` intentionally keeps two different valid states for `omp.extensions` (source paths for repo checkouts, `dist/envoy.js` for the tarball), and the workflow mutates the file in place between those states. Any pack that runs without first performing that mutation — a mis-ordered CI step, a maintainer running `bun pm pack` by hand — would otherwise silently produce a tarball whose manifest points at files (`extensions/envoy.ts`) that `"files": ["dist"]` excludes from the package entirely. `prepack.sh`'s `jq -e '.omp.extensions == ["dist/envoy.js"]'` check turns that into an immediate, loud pack failure instead of a broken artifact that only surfaces once someone tries to load the plugin.

## Prevention

- **Never mix `bun pm pack` with bare `npm publish`.** If bun is used to produce the tarball (because it correctly rewrites `workspace:*`), publish that exact tarball — `npm publish ./*.tgz`, or in CI, the path/glob to bun's pack output — and never call `npm publish` with no argument in a directory that contains any `workspace:*` reference. Clear stale tarballs first (`rm -f ./*.tgz`) so the glob can't expand to more than one path.
- **Treat internal monorepo-only packages as build-time dependencies, not install-time dependencies.** If a package is never meant to be published standalone, don't publish it standalone and don't leave it as a runtime `dependency` of something that *is* published — bundle it in and move it to `devDependencies` (or drop it from the manifest of the published package entirely once bundling stops needing the source at runtime). This turns "does npm install of this consumer package work" into a question that doesn't depend on the unpublished package's existence at all.
- **Add a pack-time guard whenever the committed manifest and the publishable manifest intentionally diverge.** Any workflow that rewrites `package.json` in place before packing (version bump, `main`/`exports`/`omp.extensions` swap to a built path) needs a `prepack` check that fails loudly if the rewrite hasn't happened, so a pack triggered outside that exact workflow step can't silently ship a manifest pointing at files the tarball doesn't contain.
- **Verify by installing the tarball outside the monorepo, not by inspecting the pack step's stdout.** `bun pm pack` succeeding and producing a plausible-looking tarball is not proof the published artifact installs correctly — the divergence in this bug was entirely between the tarball bun built and the manifest npm actually published. The only verification that would have caught it earlier is exactly what closed this bug: `npm install`/`bun add` the tarball (or the just-published registry version) into a bare directory with no workspace context, and confirm imports resolve and the entry point runs.
