---
title: Multi-agent broadcast
parent: dispatch-api
---
Planned (LEGION-233): sending one message to several live agents at once from Dispatch. Today Dispatch targets exactly one session per message, and the only bulk path is an `envoy broadcast` command outside this repository that hits every live session on one machine with no per-agent selection, no delivery mode, and no collected replies. The expected shape is a bulk-send route beside the existing targeted-message handlers plus a multi-select affordance on the web app's Agents page; how recipients are selected, whether a broadcast is one message with many deliveries or one per recipient, and how replies come back are open design questions on the issue. No code exists yet, so this component has no paths.
