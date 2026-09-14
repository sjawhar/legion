---
title: "Proving an OMP build in your own process: a loopback database is a fixture, not a rig; scrub the pane's identity; read the answer from the row"
category: testing
tags:
  - standing-order
  - smoke-rig
  - postgres
  - omp-fork
  - session-storage
  - e2e-proof
  - pr-body
date: 2026-09-14
status: active
module: proofs for packages/daemon/src/daemon/omp-pin.ts bumps; docs/kubernetes.md Session store
related_issues:
  - "LEGION-80"
  - "sjawhar/legion#1081"
symptoms:
  - "omp -p prints only the profile plugin's 'No pending work…' line; the assistant's text is not on stdout"
  - "Export named 'AckPolicy' not found in module nats when two test files run in one bun test invocation"
  - "a second `gh pr edit --body-file` from a stale local copy erased another role's E2E section"
---

# Proving an OMP build in your own process

## The standing order and what it permits

Sami (2026-09-13, 19:00Z): "Please shutdown the goddamn legion smoke. It's pointless and it has
led to destructive actions twice now." No smoke scripts, no stand-in or scratch daemon, no kind
cluster, no message-bus container, no scratch tmux server. The LEGION-19 root architect ruled
the same day that one throwaway database container on a loopback port, started in the worker's
own process purely as the database under test and removed at the end, is a **test fixture**, not
a rig. Every proof on LEGION-80 was the stock `omp` binary run directly against that fixture.

## The recipe that worked for a pin bump

```sh
unlegion=$(printenv | sed -n 's/^\(LEGION_[A-Z_]*\|DISPATCH_[A-Z_]*\)=.*/-u \1/p')
OMP='mise x github:sjawhar/oh-my-pi@<tag> -- omp'            # the tag, never `omp` from PATH
docker run -d --rm --name <issue>-pg -e POSTGRES_PASSWORD=… -p 127.0.0.1:<unused port>:5432 postgres:16
(umask 077 && printf 'postgres://…\n' > /tmp/<issue>/dsn)
cd /tmp/<issue> && env $unlegion OMP_PROFILE=legion OMP_SESSION_STORAGE=sql OMP_SESSION_SQL_DSN_FILE=/tmp/<issue>/dsn $OMP -p 'Remember the code word X. Reply with exactly OK.'
psql … -c "select path, octet_length(content), title from omp_session_files;"
env $unlegion … $OMP --resume='<row path>' -p 'What was the code word?'
docker rm -f <issue>-pg && rm -rf /tmp/<issue> ~/.omp/profiles/legion/agent/sessions/-tmp-<issue>
```

- **Scrub the pane's identity.** A child `omp` started from a Legion worker pane inherits
  `LEGION_*`/`DISPATCH_*`; the extension in it would believe it *is* this worker and exit on the
  consumed boot token. `env $unlegion` removes exactly those; keep `OMP_PROFILE=legion` so the
  real profile plugin tree (pi-legion-envoy inert without `LEGION_*`, secretsd, knives) loads as
  in production. Provider keys are already in the pane environment.
- **Name the build explicitly.** `omp` on PATH in a worker pane is `LEGION_OMP_PATH`, a
  hand-built binary. Install the tag into your own mise store with
  `mise x github:sjawhar/oh-my-pi@<tag> -- omp --version` (allowed: it is not the live profile)
  and quote `mise where` so the tag is on record.
- **Read the assistant's text from the row, not stdout.** In `-p` mode on this build the
  profile plugin's "No pending work…" line is the last thing on stdout and the assistant's own
  text is not printed. `select content from omp_session_files` and grep the JSONL for
  `"role":"assistant"` — that is where `OK` and the code word are.
- **Every refusal, with grep as the assertion.** Closed port (`127.0.0.1:1`), blank file,
  missing file, no file named, unknown value, and a libpq-form file with a marker password;
  capture stdout and stderr to files and `grep -c <marker>` both. Quote each exit code.
- **Two negative controls, not one.** Unset variables → a `.jsonl` appears and the row count is
  unchanged (the setting is doing the work). The same libpq file on the *previous* pin → the
  password on stderr (the fix is doing the work).
- **A separate container, port, and cwd per role.** Implementer, tester, and round 2 each used
  their own (`55480`/`55481`/`55482`), so no round could read another's leftovers, and each
  proof block names its own cleanup line.
- **`--mode rpc` is the daemon's real launch mode.** The tester drove `negotiate_protocol →
  prompt` over stdin and `--mode rpc --resume=<row path>`; the resolver runs before both modes,
  so `-p` proofs transfer, but say so with evidence when a daemon path depends on it. Note
  `shutdown` is not an OMP RPC command on this build — the daemon's shutdown frame is the
  worker-shim's; a bare `omp --mode rpc` ends on stdin EOF.

## Two hazards in the recording, not the running

- **`gh pr edit --body-file` from a local copy erases other roles' sections.** The PR body is a
  shared document: the tester adds `E2E (tester)`, the reviewer fills `Thermo`. Editing from a
  file you wrote hours ago twice wiped both. Fetch the live body first
  (`gh pr view --json body --jq .body`), patch your own section in place, and edit from that;
  the edit history (`userContentEdits` in GraphQL) is how the tester recovered it.
- **Two test files in one `bun test` invocation can poison each other.** `legion.test.ts`'s
  `mock.module("nats")` leaks into the daemon CLI test (`Export named 'AckPolicy' not found`);
  pre-existing on `main`, each file passes alone, CI runs packages separately. Run the
  extension suite and the daemon suites as separate invocations and say so in the handoff.
