---
title: "A file built from the final image's own state is a build-only stage FROM a named base, not a RUN in the final stage"
category: daemon
tags:
  - worker-image
  - dockerfile
  - multi-stage
  - layers
  - debian
  - third-party-notices
date: 2026-10-26
status: active
module: packages/daemon/docker
related_issues:
  - "INFRA-255"
  - "sjawhar/legion#2216"
---

# A file built from the final image's own state is a build-only stage FROM a named base, not a RUN in the final stage

- **Split the final stage where the state is complete, and read the state in a sibling stage.**
  Name the part of the final stage that produces the state (`FROM <image> AS runtime-base` holding
  the apt RUN), start the final stage `FROM runtime-base AS runtime`, and add a build-only stage
  `FROM runtime-base` that writes the file. COPY only that file into the stage that assembles it.
  The final image gains no layer and no file it would not otherwise ship, and the existing layer
  order (what reruns when) is untouched.
- **A RUN in the final stage costs either cache order or a layer.** In `worker.Dockerfile`, a RUN
  before `USER 1000:1000` sits above the probe and toolchain layers, so every change to the file
  reruns them; a RUN after the notices COPY needs `USER root` back and ships one more layer.
- **Write the rule the split depends on as what must not happen, and keep it true.** The list is
  the base image's own packages plus those the one apt RUN adds; what breaks it is a package
  installed in a later layer. "This RUN installs every package" is false for the base's ~300.
- **Keep `ENV`, `USER` and `WORKDIR` in the last stage.** `TestImageEnvIsWhatTheWorkerImageSets`
  (`packages/daemon/internal/runtime/sandbox/operatorpod_test.go`) reads only the instructions after
  the last `FROM`, so an `ENV` moved into the named base escapes it.
- **An `ARG` declared above a RUN is part of that RUN's cache key.** `ARG PI_CODEGRAPH_VERSION` sat
  above the apt RUN, so a pin bump reran `apt-get`; it now sits below it, in the final stage.
- **`dpkg-query` for a name list:** `-f '${db:Status-Status} ${Package}\n'` into a file first (dash
  has no `pipefail`, so a pipe would hide its exit status), then keep `installed` rows and
  `LC_ALL=C sort -u`. `${binary:Package}` prints `name:amd64` for `Multi-Arch: same` packages (214
  of 312 here), a name with no `/usr/share/doc/<name>` directory. `dpkg-query` over a missing
  database prints nothing and exits 0, so the consumer must refuse an empty file
  (`assemble-third-party-notices.sh` does).
- **Renaming a stage leaves comments false even when the old name stays.** The plan kept the alias
  `runtime` so that "the runtime stage" comments would stay true word for word; three of them
  described apt work that moved into `runtime-base`. Judge each hit by the work it describes, not
  by whether the name still resolves.

## Evidence

sjawhar/legion#2216: the spec asked for the `dpkg-query` read "in the image's final stage"; the plan
departed for the reasons above, and the human's approval note asked for no package, no extra layer
and no check script. Head 4d6d8cb3's image (`sha-4d6d8cb31e6a`) has 24 layers, as `main`'s
`sha-0af6288a7a9f`, with the notices COPY still between the toolchain smoke RUN and the `legion`
COPY, and no `/debian-packages`, `/tmp/dpkg-packages` or `/in/` in its filesystem (tester). The
simplify pass found the three stale "runtime stage" comments (`worker.Dockerfile` lines 22 and 262,
`docs/kubernetes.md` line 103); the retro's fresh-eyes read found the "installs every package"
comment, fixed in 08418827 by an implementing round after the first approval.
