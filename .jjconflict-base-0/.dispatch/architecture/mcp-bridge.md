---
title: Generic MCP ingestion bridge
parent: envoy
depends_on: [contracts, envoy-listener]
paths: [packages/envoy/cmd/mcp, packages/envoy/internal/mcpbridge]
---
The generic bridge from any MCP server publishing resources into Envoy's bus, and the low-maintenance default for a new source: unlike the listener's source-specific webhook handlers it stays naive about content, forwarding whatever the server publishes as an envelope. It shares the listener's bus and id packages.
