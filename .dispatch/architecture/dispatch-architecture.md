---
title: Architecture importer
parent: dispatch-server
depends_on: [dispatch-auth, dispatch-duty, dispatch-events, dispatch-model, dispatch-store, dispatch-text]
paths: [packages/envoy/internal/dispatch/architecture]
---
The importer that reads `.dispatch/architecture/*.md` from a project's source repository, validates the file set whole (`Parse` — one error naming every problem, so an invalid set is refused and the previous model stays up), and projects it into immutable snapshots and the components tables. A five-minute ticker (`duty.RunJittered`, lease `architecture-import`), jittered afresh every period, and a manual sync route drive it. Within one process, the callers of one source — one project's one repository — share one sync (`singleflight`), so two Refresh clicks cost one set of GitHub calls, and no lock is held across a GitHub call, so a slow source holds up no other source, of its project or of any other. What orders the writes of every process is the source row: each write takes the row's lock and commits only while the row's version (`xmin`) is still the one its sync loaded before fetching.
