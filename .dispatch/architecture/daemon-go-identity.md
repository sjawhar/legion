---
title: Identity and credentials
parent: daemon-go
paths: [packages/daemon-go/internal/claim]
---
Stage 2 on `main` provides the `internal/claim` role, token, and registration vocabulary, which nine of the coordinator's fifteen packages import: `cmd/legion`, `internal/api`, `internal/daemon`, `internal/runtime` with its `fake` and `tmux` implementations, `internal/store`, `internal/stream` and `internal/supervise`. Stage 3 plans daemon-held GitHub App credentials and role-bound per-command grants; Stage 5 plans the Legion service identity workers use to authenticate to Dispatch and Envoy. `internal/appauth` and `internal/credential` are those two stages' planned packages and do not exist yet.
