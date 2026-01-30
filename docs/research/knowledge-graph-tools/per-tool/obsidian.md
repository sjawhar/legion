# Obsidian - Phase 1 Screening

```yaml
tool_name: Obsidian
category: PKM / Knowledge Graph
official_url: https://obsidian.md/
pricing: freemium
pricing_details: |
  - Core app: Free (including commercial use as of Feb 2025)
  - Obsidian Sync: $4/month (end-to-end encrypted device sync)
  - Obsidian Publish: $8/month (publish notes as websites)
  - Catalyst License: $25+ one-time (optional support, early access to betas)
  - Commercial License: Optional (previously required for business use, now voluntary)
platforms:
  web_ui: no
  desktop: Windows, macOS, Linux
  mobile: both  # iOS and Android
api:
  exists: yes
  type: REST (via community plugin)
  read_write: both
  details: |
    - Official Plugin API: Internal TypeScript API for building plugins (docs.obsidian.md)
    - obsidian-local-rest-api: Community plugin providing REST API with HTTPS/API key auth
      - 1.7k GitHub stars, MIT license, last update Jan 2026
      - Full CRUD for notes, periodic notes, command execution
      - PATCH method for inserting content into note sections
      - Extension API for other plugins to add custom routes
      - OpenAPI spec available at /openapi.yaml endpoint
open_source: no
repo_url: https://github.com/obsidianmd  # Organization - API definitions and plugins only, core app is proprietary
last_commit: N/A (core app is closed source)
github_stars: 2.1k (obsidian-api repo for TypeScript definitions)
screening_result: FAIL
fail_reason: No Web UI - Obsidian is desktop/mobile only with no official web-based interface

additional_notes: |
  ## Architecture
  - Local-first: All notes stored as plain Markdown files in local "vault" folders
  - Full offline access - data never leaves device unless using optional Sync service
  - Plugin-based extensibility with 1000+ community plugins

  ## MCP Server Availability (Multiple Options)
  Obsidian has excellent MCP server support through several community implementations:

  1. **cyanheads/obsidian-mcp-server**: Comprehensive MCP bridge to Local REST API
     - Read, write, search, manage notes/tags/frontmatter
     - Acts as bridge between MCP clients and obsidian-local-rest-api plugin

  2. **otaviocc/ObsidianMCPServer**: Works with Claude, Cursor, other AI tools
     - Proofreading with grammar/text enhancement
     - Type system exploration tools

  3. **bitbonsai/mcp-obsidian**: Universal AI bridge
     - Works with Claude Desktop, Claude Code, ChatGPT Desktop (Enterprise+)
     - IntelliJ IDEA 2025.1+, Cursor IDE, Windsurf IDE
     - Safe YAML frontmatter handling

  4. **jacksteamdev/obsidian-mcp-tools**: Semantic search + Templater integration
     - AI search based on meaning/context, not keywords
     - Dynamic template execution through AI

  5. **aaronsb/obsidian-mcp-plugin**: Graph-aware AI access
     - Semantic hints and graph traversal
     - AI can explore concepts and follow connections

  ## Agent Integration Considerations
  - **Strengths**:
    - Excellent MCP ecosystem for AI agent access
    - REST API via plugin enables programmatic automation
    - Markdown-based = easy to parse and manipulate
    - Local storage = no rate limits, fast access
  - **Weaknesses**:
    - No web UI = requires desktop app running for API access
    - API depends on community plugin (not official)
    - Each user needs Obsidian running locally for agent to interact

  ## Workarounds for Web Access
  - Self-hosting via Docker (obsidian-remote container)
  - Third-party services like Neverinstall (cloud-streamed desktop app)
  - Neither are official/supported solutions

  ## Recent Updates (2025)
  - Feb 2025: Commercial license made optional (free for all work use)
  - Obsidian 1.8: Added Web Viewer core plugin (browsing web INSIDE app, not web version)
  - Obsidian 1.9: Introduced "Bases" plugin (turn notes into databases)

additional_sections:
  plugin_ecosystem:
    description: Extensive community plugin marketplace
    notable_plugins:
      - obsidian-local-rest-api (REST API access)
      - Dataview (SQL-like queries on notes)
      - Templater (advanced templating)
      - Tasks (task management)
      - Calendar, Kanban
    plugin_count: "1000+"

  data_format:
    primary: Markdown (.md files)
    metadata: YAML frontmatter
    linking: [[wikilinks]] and standard markdown links
    graph: Automatic backlink/outlink graph visualization

  sync_options:
    official: Obsidian Sync ($4/month, E2E encrypted)
    alternatives:
      - Git (obsidian-git plugin)
      - iCloud, Dropbox, Google Drive
      - Syncthing, Resilio Sync
```
