---
title: Go coordinator
parent: legion
paths: [packages/daemon-go, scripts/e2e]
---
The Go coordinator is Legion's deterministic half: admission, workflow, the issue record, process supervision across tmux and Agent Sandbox, and credentials. It is developed separately from the current TypeScript daemon until the Stage 7 cutover. `scripts/e2e` holds each stage's live proof — the daemon booting on a real Postgres, and real Oh My Pi panes under its private tmux server — the counterpart of what `scripts/kind-smoke` is to the Kubernetes runtime.
