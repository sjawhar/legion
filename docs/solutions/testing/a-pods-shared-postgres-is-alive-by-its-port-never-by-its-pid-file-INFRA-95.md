---
title: "A pod's shared Postgres is alive by its port, never by its PID file, and WebKit still cannot launch there"
category: testing
tags:
  - issue-pod
  - harness
  - postgres
  - pid-namespace
  - playwright
  - webkit
date: 2026-10-25
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-95"
  - "sjawhar/legion#2156"
---

# A pod's shared Postgres is alive by its port, never by its PID file, and WebKit still cannot launch there

Extends docs/solutions/testing/a-browser-harness-in-an-issue-pod-lives-on-the-issue-volume-INFRA-43.md.

That document says to write the harness as one idempotent script every role re-runs, and to share
the harness but not the lane. Two deltas, both paid for.

- **Guard the cluster's start on the port, never on the PID file.**
  `pg_isready -h 127.0.0.1 -p "$PG_PORT" -q` answers for any role; `pg_ctl -D <data> status` reads
  `postmaster.pid`, whose PID belongs to the container that started the server, and **each role
  container of an issue pod has its own PID namespace**. So `pg_ctl` tells every other role "no
  server running", the script starts a second postmaster, that postmaster fails to bind the port,
  removes `postmaster.pid` on its way out, and the live server shuts itself down over the lost
  data-directory lock — under whatever suite was running. The skip branch must print something a
  reader can see, for example
  `postgres is already accepting connections on 55432 (a sibling role's; left alone)`.
- **Owning a lane does not make the cluster yours.** The five ports and the database are per role
  (docs/solutions/legion/a-port-held-in-an-issue-pod-is-a-sibling-roles-harness-move-your-run-with-the-five-port-variables-and-your-own-database-and-never-stop-the-holder-LEGION-695.md,
  section "On a shared box, own the ports and the database"), but the database **server** is one
  process the whole pod shares. Liveness there is a question for the port, and nothing a role does
  to the cluster is private.
- **WebKit downloads but does not launch.** `bun run e2e:install` fetches the WebKit build onto the
  volume, and the image still lacks its system libraries
  (`libgtk-4.so.1`, `libgstreamer-1.0.so.0`, `libflite.so.1`, `libhyphen.so.0`, `libsecret-1.so.0`,
  `libGLESv2.so.2`, `libx264.so` among them), with no root to install them. A pod run covers the
  `chromium` and `iphone` projects — `iphone` is Chromium with an iPhone 13 device, not WebKit —
  and Firefox's few specs; WebKit coverage comes from CI's `dispatch` job alone. Plan the whole-suite
  leg of a pod proof around that, and say it in the evidence rather than reporting a WebKit project
  as green.

## Evidence

INFRA-95's implementer built the INFRA-43 harness in its issue pod and guarded the start with
`pg_ctl -D "$H/pgdata" status`. At 03:27 UTC on 2026-10-25 the tester re-ran the same idempotent
`setup.sh` while the implementer's lane held a live cluster: `pg_ctl` reported no server, a second
postmaster failed to bind 55432 and removed `postmaster.pid`, and the running server shut down
immediately on the invalid lock. With the guard changed to `pg_isready … -q`, the script prints its
skip line and starts nothing against a live cluster; verified against the running server, and the
whole script then re-run end to end without touching it.

The WebKit sentence is the correction of a wrong one: that implementer's first whole-suite leg
reported "51 passed, 2 skipped, 0 failed" for a group of five projects including `webkit` and
`webkit-iphone`, and the tester's run of the same group recorded 57 rows failing at
`browserType.launch` with the missing-dependency list. The decisive check is one command:
`bunx playwright screenshot --browser webkit about:blank <file>` refuses with that list and writes
no file, while the same command on `firefox` writes one.
