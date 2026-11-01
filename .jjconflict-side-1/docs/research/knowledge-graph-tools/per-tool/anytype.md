# Anytype - Phase 1 Screening

```yaml
tool_name: Anytype
category: PKM / Knowledge Graph
official_url: https://anytype.io/
pricing: freemium
pricing_details: |
  Free tier: 1GB network storage, 3 shared spaces, 3 members per shared space, local-only mode available
  Builder: ~$5/month - 128GB storage, 3 shared spaces, 10 editors per space, unique name, priority support
  Co-Creator: ~$10/month - 256GB storage, shorter unique name, ideal for teams
  Business: Custom pricing upon request - configurable storage/editors/spaces, unlimited viewers
  50% student discount available
  Self-hosting: Free (no membership required)
platforms:
  web_ui: no  # No web app; local-first architecture with desktop/mobile apps only
  desktop: yes  # Windows, macOS, Linux (Electron-based)
  mobile: both  # iOS and Android
api:
  exists: yes
  type: REST  # OpenAPI specification, runs on localhost
  read_write: both
open_source: yes
repo_url: https://github.com/anyproto/anytype-ts
last_commit: January 22, 2026  # v0.53.26-alpha release
github_stars: 6921  # 6.9k stars for desktop client
screening_result: FAIL
fail_reason: No web UI - local-first architecture requires desktop/mobile app installation

additional_notes: |
  ## Core Architecture
  - Local-first, offline-first design with peer-to-peer sync (AnySync protocol)
  - Zero-knowledge end-to-end encryption - data encrypted before leaving device
  - No cloud dependencies - can operate completely offline
  - Flexible object-based data model with types, relations, sets, and templates
  - Graph visualization for connecting objects

  ## API Details
  - Local REST API running on localhost (included with desktop app since ~2025)
  - OpenAPI specification available at developers.anytype.io
  - Authentication via 4-digit challenge in desktop app generating API key
  - Version header required: "Anytype-Version: 2025-05-20"
  - Capabilities: Global/space search, spaces/members, objects/lists, properties/tags, types/templates
  - Client libraries: Python, Go (community-maintained)
  - API is developer preview - still maturing

  ## MCP Server Available (Official)
  - Repository: https://github.com/anyproto/anytype-mcp
  - 273 stars, MIT license, v1.1.1 (January 8, 2026)
  - Official MCP server maintained by Anytype team
  - Converts OpenAPI spec into MCP tools for natural language interaction
  - Compatible with: Claude Desktop, Claude Code, Cursor, Windsurf, Raycast, LM Studio, VS Code, Cline
  - Install: `npx -y @anyproto/anytype-mcp`
  - Capabilities: Create/delete spaces and objects, search globally or within spaces, retrieve object content

  ## Real-Time Capabilities
  - AnySync: Custom peer-to-peer sync protocol
  - 20,000+ daily active users, 80,000+ monthly
  - "Virtually real-time" native sync across devices
  - Works offline with automatic conflict resolution on reconnect
  - Supports end-to-end encrypted collaboration

  ## Agent Orchestration Relevance
  - Strong: Official MCP server with full CRUD operations
  - Strong: Local API means no cloud dependencies/rate limits
  - Strong: E2EE compatible - privacy-preserving agent workflows
  - Strong: Self-hostable for complete control
  - Limitation: No web UI - cannot be accessed via browser, requires app installation
  - Limitation: Local API means each device needs its own API instance
  - Consideration: P2P architecture adds complexity vs. centralized APIs

  ## Technology Stack
  - TypeScript + Electron (desktop client)
  - Swift (iOS), Kotlin (Android)
  - Go (anytype-heart middleware, any-sync protocol)
  - gRPC for internal communication
  - Protocol Buffers for data serialization

  ## Organization Structure
  - Company: Any Association (Swiss-based)
  - GitHub org: github.com/anyproto (84 repositories)
  - Key repos:
    - anytype-ts: Desktop client (6.9k stars)
    - any-sync: Core sync protocol (1.5k stars)
    - anytype-kotlin: Android client (816 stars)
    - anytype-swift: iOS client (427 stars)
    - anytype-heart: Shared Go middleware (358 stars)
    - anytype-api: Developer portal (v1.4.5)
    - anytype-mcp: Official MCP server (273 stars)

  ## Licensing
  - Desktop client: Any Source Available License 1.0 (not OSI-approved)
  - MCP server: MIT license
  - Core protocol (any-sync): Open source
  - Note: "Any Source Available" is source-available but with some restrictions

  ## December 2025 Update
  - Anytype Chats launched (privacy-focused team workspace)
  - Concept: one space = one group = one chat
  - Forum-like discussions on objects planned

additional_sections:
  mcp_integration:
    available: yes
    server_name: anytype-mcp
    maintainer: anyproto (official)
    install: npx -y @anyproto/anytype-mcp
    license: MIT
    stars: 273
    tools_count: ~10 core operations
    key_capabilities:
      - List and create spaces
      - Global and space-specific search
      - Object CRUD operations
      - Full-text content retrieval
      - Natural language interaction

  collaboration_model:
    type: P2P with AnySync protocol
    offline_support: yes
    conflict_resolution: automatic
    e2ee: yes (zero-knowledge)
    real_time: yes (P2P sync)

  self_hosting:
    supported: yes
    license: Free (no membership required)
    docker_available: yes (any-sync-dockercompose)
    documentation: https://tech.anytype.io/how-to/self-hosting

  web_ui_status:
    available: no
    requested: yes (community feature request since 2023)
    web_clipper: yes (browser extension for saving content)
    reason: Local-first architecture prioritizes privacy over browser access
```

## Sources

- [Anytype Official Website](https://anytype.io/)
- [Anytype GitHub Organization](https://github.com/anyproto)
- [Anytype Desktop Client Repository](https://github.com/anyproto/anytype-ts)
- [Anytype Developer Portal](https://developers.anytype.io/)
- [Anytype Local API Documentation](https://doc.anytype.io/anytype-docs/advanced/feature-list-by-platform/local-api)
- [Anytype API Repository](https://github.com/anyproto/anytype-api)
- [Anytype MCP Server](https://github.com/anyproto/anytype-mcp)
- [Anytype Pricing](https://anytype.io/pricing/)
- [Anytype FAQ](https://anytype.io/faq/)
- [Anytype 2025 Roadmap Blog](https://blog.anytype.io/our-journey-and-plans-for-2025/)
- [Anytype Self-Hosting Guide](https://tech.anytype.io/how-to/self-hosting)
- [Web App Feature Request Thread](https://community.anytype.io/t/web-app-web-client/1381)
