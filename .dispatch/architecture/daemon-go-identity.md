---
title: Identity and credentials
parent: daemon-go
depends_on: [dispatch-server, envoy-listener]
paths: [packages/daemon-go/internal/appauth, packages/daemon-go/internal/claim, packages/daemon-go/internal/credential]
---
Daemon-held GitHub App credentials, role-bound per-command grants, and the Legion service identity used by workers to authenticate to Dispatch and Envoy.
