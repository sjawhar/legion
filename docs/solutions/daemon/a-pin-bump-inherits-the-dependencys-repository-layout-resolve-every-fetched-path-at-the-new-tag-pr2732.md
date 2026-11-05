---
title: "A pin bump inherits the dependency's repository layout: resolve every fetched path at the new tag"
category: daemon
tags:
  - omp-pin
  - dependency-bump
  - worker-image
  - third-party-notices
  - verification
date: 2026-11-03
status: active
module: packages/daemon
related_issues:
  - "sjawhar/legion#2732"
---

# A pin bump inherits the dependency's repository layout: resolve every fetched path at the new tag

Extends docs/solutions/daemon/omp-pin-bump-behavioral-proof.md.

- That note tells you to grep the old version string and to prove the bump by behaviour. Do one thing before either: list every path the build fetches from the dependency's repository **by tag**, and resolve each one at the **new** tag. A release can move or split a file the build names, and the version string tells you nothing about that.
- `curl -fsSL` of a moved path under `RUN set -eu` is a build failure, not a warning, so the fetch fix ships in the **same commit** as the pin. A commit carrying the pin alone produces no image at all, and the only detector is the `worker-image / build` job, which on a pull request rebuilds every layer main's cache lacks and takes about eighteen minutes. Read that job first.
- When one fetched file becomes several, split its assemble title too and keep the title/file arguments paired: a file fetched but never assembled is dropped with nothing reported.
- Name in each notices title the artifact in the image the licence text covers, not only where a copy of it lands. The upstream file says which artifact it is about; read it rather than guessing from the path.
- Fetch the notices of the components the image ships and no others. A release can publish notices for a component the image never installs.
- A hand-counted figure in a comment above the `RUN` (the number of reads below it) is part of the change: count the lines in the file and correct it, as `docs/solutions/github/a-count-in-a-workflow-comment-is-recomputed-from-the-file-never-supplied-by-eye-pr2641.md` requires.

## Evidence

sjawhar/legion#2732 raised `.omp-pin:1` from `github:sjawhar/oh-my-pi@18.13.3-sami.20261029-115123` to `@18.16.0-sami.20261103-165105`.

At the new tag the repository root holds no `THIRD-PARTY-NOTICES.txt`. Measured with `curl -fsSI` against raw.githubusercontent.com:

```
v18.13.3-sami.20261029-115123 THIRD-PARTY-NOTICES.txt                        200
v18.16.0-sami.20261103-165105 THIRD-PARTY-NOTICES.txt                        404
v18.16.0-sami.20261103-165105 packages/coding-agent/THIRD-PARTY-NOTICES.txt  200
v18.16.0-sami.20261103-165105 packages/natives/THIRD-PARTY-NOTICES.txt       200
v18.16.0-sami.20261103-165105 python/robomp/THIRD-PARTY-NOTICES.txt          200
```

`packages/daemon/docker/worker.Dockerfile` ran that fetch at `:697` under the `RUN set -eu` at `:680`, and the stage's own comment records the rule "A missing or empty file fails the build". Run through the stage's own `fetch()` at the new tag, the removed path answers `curl: (22) The requested URL returned error: 404`, status 22, with the line after it never printed.

Commit `58a357cb103d` replaced the one fetch with two, for `packages/coding-agent/` and `packages/natives/`, and the one assemble title with two (`:742-743`). `python/robomp/THIRD-PARTY-NOTICES.txt` answers 200 at the new tag and was left out: the image installs no python and no robomp. The same commit moved the stage comment's counts from seventeen reads and thirteen fetches to eighteen and fourteen.

The reviewer's own commit `0d7ecf827b42` then corrected the natives title: it named only `/home/legion/.omp/natives`, where the upstream `packages/natives/THIRD-PARTY-NOTICES.txt` says it covers "the copy embedded in the omp executable", so the title now names `/opt/omp/bin/omp` beside the extracted copy.
