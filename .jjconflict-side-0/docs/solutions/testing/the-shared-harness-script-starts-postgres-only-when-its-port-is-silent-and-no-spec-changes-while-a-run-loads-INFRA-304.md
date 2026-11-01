---
title: "The shared harness script starts Postgres only when its port is silent, and no spec changes while a run loads"
category: testing
tags:
  - issue-pod
  - harness
  - postgres
  - pid-namespace
  - playwright
date: 2026-10-26
status: active
module: packages/dispatch/e2e
related_issues:
  - "INFRA-304"
  - "sjawhar/legion#2218"
---

# The shared harness script starts Postgres only when its port is silent, and no spec changes while a run loads

Extends docs/solutions/testing/a-browser-harness-in-an-issue-pod-lives-on-the-issue-volume-INFRA-43.md.

That document tells every role to re-run one idempotent `setup.sh`, and
docs/solutions/testing/the-real-dispatch-stack-runs-inside-a-legion-pod-with-no-postgres-no-docker-and-no-root-INFRA-42.md
gives its start step as a bare `pg_ctl … -w start`. Neither links the rule that makes the re-run
safe, which
docs/solutions/testing/a-pods-shared-postgres-is-alive-by-its-port-never-by-its-pid-file-INFRA-95.md
states. Read the three together when you write the script:

- **Guard the start with `pg_isready -q -h 127.0.0.1 -p <port> -U postgres`, never
  `pg_ctl status`, and delete `postmaster.pid` only when the port is silent.** Each role's
  container has its own PID namespace, so a postmaster a sibling started looks dead to
  `pg_ctl status`; a script that then clears the PID file makes that live server shut itself
  down under the sibling's run. The pod's containers share one network namespace, so the port is
  the one answer every role sees. Pass `-U postgres`, or every probe logs
  `FATAL: role "<user>" does not exist` into the server's log.
- **Edit nothing under `packages/dispatch/e2e/` while a Playwright run is going.** The worker
  imports each spec file when it reaches it, so an edit to a shared helper mid-run gives later
  files the new helper's imports against the old module already loaded:
  `SyntaxError: The requested module './layout' does not provide an export named …` fails every
  row of those files, and the rest of the run reports a head that never existed. Read that error
  as your own edit, re-run the affected files at one head, and say which head each part of the
  run tested.

## Evidence

- INFRA-304's implementer wrote `/legion/harness/setup.sh` from the INFRA-42 and INFRA-43
  recipes with a `pg_ctl status` guard, one day after INFRA-95 had recorded the same kill. The
  tester's run of it removed `postmaster.pid` and the implementer's Postgres shut down
  (`received immediate shutdown request … could not open file postmaster.pid`). With the guard
  changed to `pg_isready`, the implementer ran `setup.sh` from its own container against the
  Postgres the tester's container had started (invisible to the implementer's `ps`): the server
  still answered 70 seconds later, and `postmaster.pid` was untouched.
- The implementer applied the simplify pass to `e2e/layout.ts` during a 47-minute whole-project
  `--project=iphone` run: 618 passed, and `phone.e2e.ts` had 1 failure and 6 rows that did not
  run, all on that `SyntaxError`. Re-running `phone.e2e.ts` and `agent-view.e2e.ts` at one head
  gave 19 passed; the whole project was not run again at one head.
