---
title: "A re-runnable setup block builds into .new and renames last, and the daemon it starts holds neither its lock nor its library path"
category: testing
tags:
  - harness
  - setup-script
  - flock
  - ld-library-path
  - signals
date: 2026-10-29
status: active
module: scripts/dispatch
---

# A re-runnable setup block builds into .new and renames last, and the daemon it starts holds neither its lock nor its library path

Extends docs/solutions/testing/a-browser-harness-in-an-issue-pod-lives-on-the-issue-volume-INFRA-43.md,
which asks for a setup script that "rebuilds only what is missing". These rules make "present"
mean "complete".

- **Build each directory that a later run checks only for existence under `<name>.new`, and rename
  it last.** Run `rm -rf <name>.new`, build into it, then `mv <name>.new <name>`. This applies to
  extracted binaries and to an `initdb` cluster: a Postgres data directory can move while its
  server is stopped. An interrupted run then leaves only a `.new` directory, which the next run
  removes. After a failure, tell the reader to remove the whole root and run the block again.
- **Close the lock descriptor on the command that starts a daemon.** In
  `( flock 9 && … ) 9>"$dir/setup.lock"`, every child inherits descriptor 9, and a daemon outlives
  the subshell. Write `pg_ctl … start 9>&-`, or every later run waits on the lock for as long as
  the daemon runs.
- **Never export `LD_LIBRARY_PATH` to a downloaded bundle's `lib/`.** An exported path reaches
  every program the block runs, the system `python3` that extracts the bundle among them. zonky's
  `initdb`, `pg_ctl` and `postgres` carry `RUNPATH $ORIGIN/../lib` and need no library path. When
  a binary needs one, set it on that one command.
- **Send a Ctrl-C test's SIGINT to a process that does not ignore it.** A background job of a
  non-interactive shell starts with SIGINT ignored, and `python3` keeps it ignored. Start the run
  in its own process group with SIGINT at its default action, for example
  `python3 -c 'import os, signal, sys; signal.signal(signal.SIGINT, signal.SIG_DFL); os.setsid(); os.execvp("bash", ["bash", "-c", "source " + sys.argv[1]])' <block> &`.
  Then send `kill -INT -- -<pid>` once the step you test has started. `initdb` installs its own
  handlers, so a test that interrupts only `initdb` can pass while the extraction step was never
  interrupted.

## Evidence

sjawhar/legion#2476, `scripts/dispatch/README.md`, section "Lexeme database".

- At da02c91a the started postmaster held the lock: `/proc/<pid>/fd/9 -> setup.lock`. A second run
  waited on `flock` until the 600 s command timeout. With `9>&-`, two first runs at once left one
  server, and the second run printed the socket path.
- At da02c91a the block exported `LD_LIBRARY_PATH="$pg/server/lib"` before it extracted the
  binaries. After a run that was interrupted inside `initdb`, the rerun failed with
  `Bus error  python3 -c '… extractall("server", filter="data")'`, and every later run failed
  with `liblzma.so.5: file too short`. A SIGKILL inside `initdb` left `data/` with no
  `global/pg_control`. The rerun skipped `initdb` and failed with
  `pg_ctl: error: could not open file "data/global/pg_control" for reading`.
- At 75e39fdb, with `server.new` and `data.new` and no export, these four cases each left a rerun
  that exited 0 with one server: SIGINT and SIGKILL during `initdb`, and SIGINT and SIGKILL during
  extraction. `readelf -d` on the three binaries shows `RUNPATH $ORIGIN/../lib`.
- The first SIGINT test of the extraction ran as a plain background job, and the extraction
  finished. Started with SIGINT at its default action, the same test ended run 1 with exit 130 and
  `KeyboardInterrupt` in `tarfile`.
