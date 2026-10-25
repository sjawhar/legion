---
title: "A scratch service on the issue volume survives a container replacement but does not restart: its data directory comes back group-readable"
category: testing
tags:
  - issue-pod
  - postgres
  - harness
  - agent-sandbox
  - recovery
date: 2026-10-25
status: active
module: scripts/e2e
related_issues:
  - "INFRA-251"
  - "sjawhar/legion#2163"
---

# A scratch service on the issue volume survives a container replacement but does not restart

Extends `docs/solutions/testing/a-browser-harness-in-an-issue-pod-lives-on-the-issue-volume-INFRA-43.md`,
which says to build the harness on `/legion` because the issue volume survives a pod replacement
and the container filesystem does not. True, and not the whole story: the **files** survive, the
**process** does not, and bringing it back is not just re-running the start command.

- **A role's container can be replaced mid-issue**, between one turn and the next, with no notice
  in the session: a new generation of the role starts in the same workspace. Every service that
  role started — a database, a broker, a tmux server — is gone with the old container, while its
  data directory sits intact on the volume.
- **Restart it, do not rebuild it.** A fresh `initdb` over a surviving data directory costs the
  state the round's evidence rests on; the start command alone is the right move.
- **PostgreSQL refuses a data directory whose group can read it.** The volume's files come back
  with group permissions, and `pg_ctl` then stops with `could not start server` while the only
  explanation is in the log file: `data directory "<dir>" has invalid permissions`, `Permissions
  should be u=rwx (0700) or u=rwx,g=rx (0750)`. Read the log before concluding the rig is lost.
  `chmod 0700 <dir>` and the same `pg_ctl` line bring it back with the database intact.
- **Say in the handoff where the rig is and how to revive it.** The next role of the issue shares
  the pod's network namespace, so a service on `127.0.0.1` is reachable from its container too —
  but only if it is running, and only if that role knows the directory and the one `chmod`.

## Evidence (INFRA-251, sjawhar/legion#2163)

The implementer's rig for this issue is a portable PostgreSQL 17.2 under
`/legion/scratch/rig251`, which gave every round its real-database proof. Between two turns of one
hold the implementer's container was replaced (the issue record's claim moved to incarnation
`…/2`). The next `go test` run failed every case with
`dial tcp 127.0.0.1:55432: connect: connection refused`, and `pg_ctl … start` answered
`waiting for server to start.... stopped waiting / pg_ctl: could not start server / Examine the log
output.` The log's last two lines were

```
FATAL:  data directory "/legion/scratch/rig251/data" has invalid permissions
DETAIL:  Permissions should be u=rwx (0700) or u=rwx,g=rx (0750).
```

`chmod 0700 /legion/scratch/rig251/data` and the original `pg_ctl` line started it with every
database still there, and the round's selections ran green against it minutes later. Nothing was
rebuilt and no evidence was recomputed from a fresh database.
