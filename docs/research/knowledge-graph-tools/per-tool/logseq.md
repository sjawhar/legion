# Logseq - Phase 1 Screening

```yaml
tool_name: Logseq
category: PKM / Knowledge Graph
official_url: https://logseq.com/
pricing: open_source
pricing_details: |
  Core app is free and open-source (AGPL-3.0). Optional Logseq Sync service
  available to Open Collective supporters starting at $5/month. Logseq Pro
  (with real-time collaboration and sync with own storage) announced but
  pricing not finalized.
platforms:
  web_ui: limited
  desktop: Windows, macOS, Linux, BSD
  mobile: both
api:
  exists: yes
  type: HTTP (local server) + Plugin SDK
  read_write: both
open_source: yes
repo_url: https://github.com/logseq/logseq
last_commit: 2026-01-30
github_stars: 40600
screening_result: FAIL
fail_reason: |
  No true web UI - only a demo/test version exists (https://demo.logseq.com/
  and https://test.logseq.com/). The web version has been stuck on an old
  version (0.6.3) and development focus has shifted to desktop apps. Users
  cannot run full-featured Logseq in a browser with their own data.

additional_notes: |
  ## Overview
  Logseq is a privacy-first, open-source platform for knowledge management
  and collaboration. It uses a block-based outliner approach with bidirectional
  linking and graph visualization. Supports Markdown and Org-mode file formats.

  ## API Architecture
  - **Local HTTP API Server**: Runs at localhost:12315 by default. Must be
    enabled in Settings > Features > "Enable HTTP APIs server"
  - **Authentication**: Token-based via Authorization header (Bearer token)
  - **Main Endpoint**: POST /api - invokes Plugin SDK methods
  - **Plugin SDK**: Full documentation at https://plugins-doc.logseq.com/
  - **Read/Write**: Full CRUD operations on pages and blocks

  ## MCP Server Availability (Critical for AI Agent Orchestration)
  Multiple MCP servers exist for Claude integration:
  - mcp-logseq (by ergut): https://github.com/ergut/mcp-logseq
  - logseq-mcp-tools (by joelhooks): https://github.com/joelhooks/logseq-mcp-tools
  - Logseq MCP v4.0: Complete page/block/TODO management

  MCP capabilities include:
  - Page operations: create, read, update, delete, list with filters
  - Block management: insert, update, delete, get by UUID
  - TODO management: organized by status (TODO, DOING, DONE)
  - Advanced search with templates, properties, relations, date filters
  - All operations are local-only (privacy-first)

  ## Web UI Limitations
  The web version is essentially deprecated:
  - demo.logseq.com exists but is read-only demo
  - test.logseq.com is early alpha for upcoming Logseq DB version
  - No sync support in browser version
  - Development focus has shifted away from web to desktop/mobile
  - Many users requesting PWA but not implemented

  ## Technical Stack
  - Primary language: Clojure (66.3%)
  - Desktop: Electron-based
  - Mobile: Native iOS and Android apps
  - Storage: Local filesystem (Markdown/Org-mode files)
  - Graph database internally for semantic connections

  ## Real-time Capabilities
  - Real-time collaboration (RTC) is in beta testing (as of Dec 2025)
  - Currently available to sponsors/backers for testing
  - Sync service syncs every 2 seconds on mobile, 20 seconds on desktop

  ## Strengths for AI Agent Use
  - Excellent MCP ecosystem with multiple mature server implementations
  - Local-first architecture means full data control
  - Block-level granularity for precise content manipulation
  - Built-in task/TODO system
  - Active development (commits daily)
  - Large community (40k+ GitHub stars)

  ## Limitations for AI Agent Use
  - No web UI means cannot be accessed programmatically via browser automation
  - Local HTTP API requires desktop app to be running
  - No cloud/hosted version - must run locally
  - API requires app to be open (no headless mode officially supported)

additional_sections:
  file_format_support:
    - Markdown
    - Org-mode

  plugin_ecosystem:
    marketplace: yes
    plugin_api_docs: https://plugins-doc.logseq.com/
    notable_plugins:
      - logseq-copilot (browser extension for web page capture)
      - integrate-any-api (connect external APIs)

  sync_options:
    - Logseq Sync (paid, $5+/month)
    - Git/GitHub
    - Syncthing
    - iCloud/Dropbox (manual setup)

  community_resources:
    forum: https://discuss.logseq.com/
    documentation: https://docs.logseq.com/
    hub: https://hub.logseq.com/
```
