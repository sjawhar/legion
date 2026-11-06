---
title: "A pod whose Bun lags the pin builds the Claude plugin with the pinned release from scratch"
category: legion
tags:
  - bun-version
  - claude-envoy
  - committed-bundle
  - worker-pod
date: 2026-10-28
status: active
module: packages/claude-envoy
related_issues:
  - "sjawhar/legion#2366"
---

# A pod whose Bun lags the pin builds the Claude plugin with the pinned release from scratch

Extends docs/solutions/legion/committed-plugin-bundles-inline-contracts-so-every-contracts-change-carries-a-dist-commit.md.

- When `bun scripts/build.ts` in `packages/claude-envoy` refuses with `refusing to build under Bun
  <x>: .bun-version pins <y>`, the pod's image carries an older Bun than main now pins. Do not
  resolve conflicted `dist/*.js` files by hand, and do not build them with the older Bun.
- Download the pinned release into the issue's scratch:
  `curl -fsSL -o $TMPDIR/bun/bun.zip https://github.com/oven-sh/bun/releases/download/bun-v$(cat .bun-version)/bun-linux-x64-baseline.zip`.
  The image has no `unzip`, so extract it with Python:
  `python3 -c "import zipfile; zipfile.ZipFile('$TMPDIR/bun/bun.zip').extractall('$TMPDIR/bun')"`.
  Then make the binary executable.
- Run that binary by its path for every command whose output is committed: `install --frozen-lockfile`,
  `scripts/build.ts`, `scripts/build.ts --check`, and the bun tests CI runs. `bun run …` resolves
  `bun` from `PATH` and gets the old one.

## Evidence

The forward merge fb909998 of sjawhar/legion#2366 onto a main that pinned Bun 1.4.4 conflicted in
the three `packages/claude-envoy/dist/*.js` bundles, while the pod ran Bun 1.3.14. Building with the
release fetched as above gave `dist/ and legion/ match a fresh build`.
