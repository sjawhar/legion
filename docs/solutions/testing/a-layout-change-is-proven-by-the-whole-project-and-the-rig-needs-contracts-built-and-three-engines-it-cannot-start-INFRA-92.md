---
title: "A layout change is proven by the whole project, and the pod's browser rig needs contracts built, its ports free, and three engines it cannot start"
category: testing
tags:
  - playwright
  - issue-pod
  - harness
  - layout
  - dispatch
date: 2026-10-25
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-92"
  - "sjawhar/legion#2154"
---

# A layout change is proven by the whole project, and the pod's browser rig needs contracts built, its ports free, and three engines it cannot start

Extends docs/solutions/testing/a-browser-harness-in-an-issue-pod-lives-on-the-issue-volume-INFRA-43.md.

That document builds the rig. This is the delta a change to a **rendered page** pays on top of
it: what to run, what a pod cannot run at all, and what the run costs.

- **A layout change is proven by the whole project, never by `-g` on the row you wrote.** A row
  you add measures the element you were thinking about; a layout change touches every row of the
  page. Run the project.
- **A page assertion names a rendered box, not text.** `toHaveText` and `toHaveAttribute` both
  succeed on an element rendered at zero width, where nothing is painted and no hover can reach
  it. Assert `toBeVisible`, a `boundingBox()` width, and `assertWhole` (`e2e/unclipped.ts`) where
  the text is meant to be read.
- **Build `@legion/contracts` before Playwright loads the specs:** `cd packages/contracts && bun
  run build` (0.13 s). Without it three specs die on `Cannot find module …/@legion/contracts/dist/index.js`
  — after the web bundle and the whole harness have started. CI builds it as a matter of course,
  so nothing in the suite's own documentation says it.
- **A run killed by a deadline leaves its five listeners holding their ports.** The next run is
  refused at config load naming all five. Kill them; do not move the ports, which points
  `e2e/seed.ts`'s truncation at another database.
- **Three of the seven Playwright projects cannot start in a worker pod at all.** `webkit`,
  `webkit-iphone`, `firefox` and `firefox-download-navigation` fail host validation on system
  libraries no agent can install (`libgtk-4`, the gstreamer set, `libflite`), and there is no
  root. Run `chromium`, `iphone` and `chromium-plain-http` locally and say plainly in the handoff
  that CI carries the rest.
- **Budget the run.** A pod is roughly three times slower than CI at this suite: the `chromium`
  project alone took about fifty minutes here against CI's forty-two for everything. A command
  deadline under an hour cuts it mid-project.
- **A tool the rig needs lives on the issue volume, not in the container.** A worker container's
  `/home` does not survive a pod replacement: a `nats-server` installed into `GOPATH`'s `bin` and
  used by three smoke runs was gone by the next round, and the smoke failed with `command not
  found` through a listener that then died of `nats: no servers available`. `/legion/scratch/bin`
  survives; so does the rig the earlier document puts there.
- **The dev-sign-in server refuses five things, not one.** Point the rig at anything off-machine
  and you meet whichever comes first, not the one you expect: a non-loopback listen host
  (`packages/envoy/cmd/dispatch/main.go:686`), a non-loopback `DATABASE_URL` (`:688`, refused in
  `loopbackDatabase` at `:741-752`), a set `DISPATCH_SIGNING_KEY` (`:692`), a remote NATS (`:695`),
  a non-loopback agent-secrets URL (`:699`), the real App private key on disk (`:730`), and a
  non-loopback GitHub API base (`:733`).

## Evidence

INFRA-92 changed the Agents row so the session name takes the row's width. Two filtered runs
(`-g "an agent row shows the whole session name"`) were green while the suite was red: ten rows
in `keyboard-agents-pin.e2e.ts` and `keyboard-agents-picker.e2e.ts` — specs the change never
touched — matched a row by its whole directory text, which the row no longer rendered above `md`.
The same round's "before" CI run at the base failed three other rows and none of these, so the
diff was the cause.

The next round's defect was invisible to text assertions: beside the 64-character name the
directory rendered 0 × 16 px and not visible, while `toHaveText` and `toHaveAttribute` passed and
the `dispatch` job was green. The tester's red row measured the box instead.

Run shapes that worked here: the whole `chromium` project, 721 passed, 0 failed; `iphone` and
`chromium-plain-http` over the thirteen specs the change touched, 98 passed, 0 failed.
