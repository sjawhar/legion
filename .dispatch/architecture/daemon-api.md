---
title: Coordinator API and CLI
parent: daemon
depends_on: [daemon-state, daemon-supervision, daemon-identity]
paths: [packages/daemon/cmd/legion, packages/daemon/internal/api, packages/daemon/internal/config, packages/daemon/internal/daemon]
---
Stages 1 and 2 on `main` provide the `legion` command (including `worker-shim` and `claims`), configuration, boot and shutdown assembly, the state and health routes, and the claim-registration and operator routes. Stages 3 and 4 complete the agent API and CLI commands. Its API compatibility with the language-neutral contracts is not a Go source dependency.

`legion.yaml`'s top-level `repository_renames` (a list of `from`/`to` pairs) settles onto `config.Config.RepositoryRenames`, the `ghrepo.Renames` every consumer takes; `legion start --check-config` prints one line per pair after its `Config OK:` line, and `api.SettingsOf` carries them as `settings.repository_renames`, so `legion state --json` reads back what the daemon treats as one repository.

`legion.yaml`'s `projects.<KEY>.memory` (`mode`, `config_files`, `instructions`) settles onto `config.Project.Memory`, which boot reads once and every launch of a tree in the memory group takes through `runtime.SpawnSpec.Memory`; `api.SettingsOf` carries the block as `settings.projects.<KEY>.memory`, and `GET /legion/v1/state` carries on every issue its tree's `memoryGroup`, `memoryDrawMode` and `memoryDrawnAt`, the three absent together while nothing has drawn for that tree (`api.Issue`, daemon API contract 47; a caller in a pod whose claims record a contract below 47 is served the state without them, `stateFor`'s `withoutMemory`). So `legion state --json` reads back which trees were drawn, under which mode and when, and `legion state` prints one `memory:` line counting them by group and mode, naming no issue, since the state names no tree.
