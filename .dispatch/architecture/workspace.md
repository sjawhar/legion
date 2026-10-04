---
title: Workspace provisioning
parent: daemon
depends_on: [daemon-supervision]
paths: [packages/daemon/internal/workspace]
---
Issue workspace provisioning for phase workers: the shared clone of each repository and a jj workspace of it per issue, with the repository credential. The daemon runs it on its own host; a Sandbox pod runs it as `legion workspace-init` in its init containers.
