---
title: Envoy client
parent: legion
depends_on: [contracts]
paths: [packages/envoy-client]
---
Shared TypeScript HTTP layer and `dispatch` command for every agent host: Dispatch config resolution, command execution, the text of each result (`dispatch search` prints each ask hit's answer state under it), inbound envelope rendering, the relay protocol of consumer delivery (`consumer-delivery`: `received` at decode, `done` once the host wrote the frame, the resume point), and the replay of what a session missed on the core path (`replay`).
