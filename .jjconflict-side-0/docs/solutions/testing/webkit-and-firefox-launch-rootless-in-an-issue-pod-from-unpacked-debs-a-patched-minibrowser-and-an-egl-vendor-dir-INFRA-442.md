---
title: "WebKit and Firefox launch rootless in an issue pod, from unpacked debs, a patched MiniBrowser and an EGL vendor directory"
category: testing
tags:
  - playwright
  - webkit
  - firefox
  - issue-pod
  - harness
  - dispatch
date: 2026-10-26
status: active
module: packages/dispatch/e2e
supersedes:
  - docs/solutions/testing/a-pods-shared-postgres-is-alive-by-its-port-never-by-its-pid-file-INFRA-95.md
  - docs/solutions/testing/a-layout-change-is-proven-by-the-whole-project-and-the-rig-needs-contracts-built-and-three-engines-it-cannot-start-INFRA-92.md
  - docs/solutions/testing/a-mutation-that-stays-green-because-the-platform-implements-the-behaviour-too-INFRA-43.md
related_issues:
  - "INFRA-442"
  - "sjawhar/legion#2227"
---

# WebKit and Firefox launch rootless in an issue pod, from unpacked debs, a patched MiniBrowser and an EGL vendor directory

This supersedes only one claim in each of the three files above: that WebKit (and, in INFRA-92, Firefox) cannot launch in a worker pod, so their rows are CI's alone. Everything else in them stands.

## Rule

Run `webkit`, `webkit-iphone` and Firefox rows in the pod rather than leaving them to CI. On top of
the harness in
docs/solutions/testing/a-browser-harness-in-an-issue-pod-lives-on-the-issue-volume-INFRA-43.md:

1. **Fetch only what the image lacks, without root.** Point `APT_CONFIG` at a file of your own:
   `Dir::Etc::Parts` an empty directory (this also skips the image's docker-clean hooks),
   `Dir::State` and `Dir::Cache` on the volume, `Dir::State::status "/var/lib/dpkg/status"` (the
   image's own, so dependencies resolve against what is installed), `Debug::NoLocking "1"` and
   `APT::Sandbox::User` set to `$(id -un)`. Then run `apt-get update` and
   `apt-get install -d --no-install-recommends <Playwright's debian13 WebKit, Firefox and Chromium
   lists, plus its font set>`, and run `dpkg-deb -x` on each `.deb` into a root on the volume.
2. **Patch the MiniBrowser wrapper itself.** `webkit-*/minibrowser-*/MiniBrowser` overwrites
   `LD_LIBRARY_PATH`, so an exported value never reaches WebKit. Append the root's
   `usr/lib/x86_64-linux-gnu` to its `export LD_LIBRARY_PATH=` line, guarded by a marker comment so
   a re-run does nothing, and fail loudly if the line is not found (a new Playwright revision).
3. **Give headless WebKit an EGL display.** The image ships glvnd and Mesa's driver but no vendor
   file, and WPE dies with `Could not create EGL display: no supported platform available`
   (visible only under `DEBUG=pw:browser*`). Export
   `__EGL_VENDOR_LIBRARY_DIRS=<root>/usr/share/glvnd/egl_vendor.d`, plus `FONTCONFIG_FILE` (a
   fonts.conf including `/etc/fonts/fonts.conf` and `<root>/usr/share/fonts`) and the
   `GST_PLUGIN_SYSTEM_PATH_1_0`, `GST_PLUGIN_SCANNER_1_0` and `GST_REGISTRY_1_0` variables
   pointing into the root and the volume.
4. **Prove the engine yourself.** Playwright's host check runs `ldd` against system paths only and
   refuses WebKit even once its libraries resolve, so set
   `PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=1`. That variable also hides a library that really
   is missing, so check two things in its place: every file under `webkit-*/minibrowser-wpe` must
   show no `not found` in `ldd` with the wrapper's library path, and
   `bunx playwright screenshot --browser <engine> about:blank <file>` must write the file for
   chromium, webkit and firefox. Read the counts line of the real run
   (docs/solutions/testing/read-a-test-runners-counts-line-never-a-tail-of-its-failure-list-INFRA-95.md).

Keep the script and its `env.sh` on the issue volume, sourced only around harness commands; they are
not committed, so write them again from these steps in a new pod.

## Evidence

- INFRA-442 built this at `/legion/scratch/harness` (`setup.sh` idempotent, `env.sh` keyed on
  `LEGION_ROLE`). Debian's apt resolved 232 missing packages against the image's dpkg status, and
  the container's own disk stayed at about 13 MB used.
- With it, `e2e/agent-view.e2e.ts e2e/btw.e2e.ts e2e/inbox-drawer.e2e.ts --project chromium
  --project iphone --project webkit-iphone` gave 50 passed and 10 skipped; `e2e/phone-conversation.e2e.ts
  e2e/delivery-live.e2e.ts --project webkit-iphone` gave 5 passed. The tester's own probe rows passed 32/32 across Desktop
  Chrome, Desktop Safari (WebKit), Desktop Firefox and iPhone 13 (WebKit) (sjawhar/legion#2227).
- Before the EGL variable, the libraries resolved and WebKit still exited at launch. Before the
  wrapper patch, exporting `LD_LIBRARY_PATH` changed nothing.
