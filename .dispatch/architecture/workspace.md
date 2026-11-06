---
title: Workspace provisioning
parent: daemon
depends_on: [daemon-supervision]
paths: [packages/daemon/internal/workspace]
---
Issue workspace provisioning for phase workers: the shared clone of each repository and a jj workspace of it per issue, with the repository credential. The daemon runs it on its own host; a Sandbox pod runs it as `legion workspace-init` in its init containers. The same package also clones the deployment's skills marketplace (`runtime.kubernetes.skills_repository`) into a pod volume in an init container of its own, with the same provisioning credential, so no role's launch fetches anything, and records the digest of each plugin's tree that a role's own `legion skills-check` compares its installed plugin against.
