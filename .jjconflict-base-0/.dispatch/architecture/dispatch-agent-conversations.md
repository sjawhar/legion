---
title: Agent conversation interface
parent: legion
---
Planned (LEGION-232): a real conversation view for a live agent in Dispatch. Today the Agents page shows only the targeted exchanges a human sent a session (`btw`, `aside`, `steer`) and its replies, never the session's own turns, tool calls, reasoning or streaming output. Two decisions are settled: build on **assistant-ui** rather than write a client, and **stream live without storing** — a session's content may pass through the Dispatch server to an open page but is never written to its database, leaving the session's own transcript on its host as the record. The expected shape is the `pi-envoy` extension streaming a session's events through the Envoy listener to the Dispatch server, which serves them to the web app; talking back reuses the existing delivery modes. No code exists yet, so this component has no paths.
