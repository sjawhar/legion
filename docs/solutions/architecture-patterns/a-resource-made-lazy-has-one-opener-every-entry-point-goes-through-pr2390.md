---
title: "A resource made lazy has one opener every entry point goes through"
category: architecture-patterns
tags:
  - lazy-initialisation
  - operator-commands
  - nats
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/redeliver
---

# A resource made lazy has one opener every entry point goes through

When a resource a struct used to receive opened (a KV bucket, a client) becomes lazily opened
because its dependency may be down at boot:

- **Make the opener the only way to the resource.** Remove the field that held the opened value
  from the struct's exported surface; cache it in an unexported field filled by the opener, which
  the method that uses it calls first.
- **Leave no field that is sometimes set and sometimes zero.** A caller that builds the struct the
  old way, or reads the field before the opener ran, gets a zero value and a nil dereference.
- **Run every entry point that builds the struct**, not only the server: operator subcommands and
  integration tests build it themselves and never run the server's loop that opened it.

## Evidence

- `redeliver.Sweeper` gained an `Open` function beside its `State` field so the server could start
  with NATS down; only the server's loop turned `Open` into `State`. The operator command
  `redeliver-webhooks` (`packages/envoy/cmd/dispatch/redeliver.go`) called `Sweep` directly and
  panicked on the zero `State`. The simplify pass found it (fixed in 551e02b3, with
  `TestASweepOpensTheStateItWasBuiltWithout` and `TestASweepWhoseStateWillNotOpenFails`).
- Review then made `Open` the one source: `Sweep` calls `openState` (`internal/dispatch/redeliver/sweep.go`,
  line 671) before any reader, and the exported `State` is gone (f49a7099);
  `internal/integration` had to switch to `Open` too.
