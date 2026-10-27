---
title: Claude Code plugin
parent: legion
depends_on: [envoy-client, contracts]
paths: [packages/claude-envoy]
---
The Claude Code channel plugin that delivers Envoy and Dispatch traffic into Claude Code sessions.

Its committed `dist/` bundles the `dispatch` command from `envoy-client`, so a change to the command's output, such as the related-item check's three forms or `dispatch related`, is rebuilt into it in the same change.
