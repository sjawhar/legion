---
title: Claude Code plugin
parent: legion
depends_on: [envoy-client, contracts]
paths: [packages/claude-envoy]
---
The Claude Code channel plugin that delivers Envoy and Dispatch traffic into Claude Code sessions. Its `dist/` bundles the envoy-client `dispatch` command, so a change there, such as `dispatch search` printing each ask hit's answer state, reaches Claude Code only with `dist/` rebuilt.
