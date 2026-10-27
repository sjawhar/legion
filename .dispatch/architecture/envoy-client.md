---
title: Envoy client
parent: legion
depends_on: [contracts]
paths: [packages/envoy-client]
---
Shared TypeScript HTTP layer and `dispatch` command for every agent host: Dispatch config resolution, command execution, inbound envelope rendering, and the list of what people wrote to a session in the last 72 hours (`src/session-answers.ts`), which every host carries on each model request within an 800 ms deadline from one process-wide read and a memory and disk cache, and which `dispatch answers` prints in full.
