# Heptabase - Phase 1 Screening

```yaml
tool_name: Heptabase
category: PKM / Knowledge Graph
official_url: https://heptabase.com/
pricing: paid_only
pricing_details: |
  Pro Plan: $8.99/month (billed yearly) or $11.99/month
  Premium Plan: $17.99/month (billed yearly)
  7-day free trial available (no free tier)
  Premium includes 10x AI credits, unlimited PDF uploads, access to OpenAI/Anthropic/Gemini models
platforms:
  web_ui: yes
  desktop: Mac (Intel + Apple Silicon), Windows, Linux
  mobile: both
api:
  exists: yes
  type: MCP (Model Context Protocol)
  read_write: both
  endpoint: https://api.heptabase.com/mcp
open_source: no
repo_url: https://github.com/heptameta (organization - no main app source)
last_commit: 2026-01-28 (project-meta repo)
github_stars: 113 (project-meta repo only)
screening_result: PASS
fail_reason:

additional_notes: |
  ## MCP API Details
  Heptabase provides an official MCP (Model Context Protocol) server with OAuth authentication.

  ### Read Operations (7 tools):
  - semantic_search_objects: Search knowledge base using keywords and semantic matching
  - search_whiteboards: Locate whiteboards by name and topic keywords
  - get_whiteboard_with_objects: Retrieve full whiteboard structure and relationships
  - get_object: Access complete content from notes, journals, media cards
  - get_journal_range: Retrieve journal entries across date ranges (up to ~3 months per request)
  - search_pdf_content: Search within PDFs using keyword matching (returns up to 80 ranked chunks)
  - get_pdf_pages: Retrieve specific page ranges from PDFs

  ### Write Operations (2 tools):
  - save_to_note_card: Creates new note cards in inbox from AI responses
  - append_to_journal: Adds content to today's journal entry (auto-creates if needed)

  ## AI Agent Orchestration Relevance
  - Official MCP endpoint makes it highly compatible with Claude, ChatGPT, and MCP-supporting tools
  - OAuth-based authentication for secure access
  - Write capabilities allow agents to store outputs directly into the knowledge base
  - Semantic search enables meaning-based retrieval (not just keyword matching)
  - PDF search and retrieval useful for research workflows

  ## Third-Party MCP Server
  Community-built MCP server (LarryStanley/heptabase-mcp) works with backup data locally.
  Features: export to Markdown/JSON/Mermaid, relationship analysis, offline-first.

  ## Limitations for Agent Use
  - Write operations limited to inbox cards and journal entries (cannot modify existing cards or whiteboards)
  - No traditional REST API - MCP only
  - Paid-only pricing may be a barrier for experimentation
  - Current version: 1.83.5

  ## Platform Details
  - Web app available at app.heptabase.com (full-featured, launched January 2025)
  - Desktop apps: macOS Intel, macOS Apple Silicon (M1/M2/M3/M4), Windows, Linux (AppImage)
  - Mobile apps: iOS (App Store), Android (Google Play) - full-featured
  - Web Clipper browser extension available

additional_sections:
  mcp_integration:
    official_server: yes
    endpoint: https://api.heptabase.com/mcp
    authentication: OAuth
    compatible_clients:
      - Claude Desktop
      - Claude Code
      - ChatGPT
      - Cursor
      - Other MCP-compatible tools
  visual_knowledge_features:
    whiteboards: yes
    bidirectional_links: yes
    card_library: yes
    pdf_annotations: yes
    ai_summaries: yes
  real_time_capabilities:
    cross_device_sync: yes
    collaborative_editing: limited (collaborator invites available)
```

## Sources

- [Heptabase Official Website](https://heptabase.com/)
- [Heptabase Pricing](https://heptabase.com/pricing)
- [Heptabase Download](https://heptabase.com/download)
- [Heptabase MCP Documentation](https://support.heptabase.com/en/articles/12679581-how-to-use-heptabase-mcp)
- [Heptabase GitHub Organization](https://github.com/heptameta)
- [Heptabase 1.0 Announcement](https://wiki.heptabase.com/version-one)
- [Community MCP Server - LarryStanley/heptabase-mcp](https://github.com/LarryStanley/heptabase-mcp)

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://support.heptabase.com/en/articles/12679581-how-to-use-heptabase-mcp
  auth_model: oauth
  rate_limits: Not documented publicly
  webhooks: no
  subscriptions: no  # No event notification system
  mcp_server: official
  mcp_server_url: https://api.heptabase.com/mcp
  community_mcp_server: https://github.com/LarryStanley/heptabase-mcp

data_model:
  node_structure: |
    Cards are the fundamental unit. Seven card types exist:
    - Note Cards: Text-based knowledge and ideas
    - Journal Cards: Date-specific entries
    - Highlight Cards: Excerpts and annotations
    - PDF Cards: Document containers
    - Video Cards: Video file containers
    - Audio Cards: Audio file containers
    - Image Cards: Image file containers
    All cards live in the global Card Library, not in whiteboards.
    Whiteboards are thinking spaces that reference cards (same card can appear on multiple whiteboards).
  schema_flexibility: dynamic  # Tags provide dynamic typing with customizable properties
  custom_fields: yes  # 9 property types: text, number, select, multi-select, date, checkbox, URL, phone, email
  relations: |
    - Bi-directional links: Type @ to mention other cards, creates block-level backlinks
    - Whiteboard placement: Cards track which whiteboards contain them
    - Visual connections: Arrows can connect cards on whiteboards
    - Tag-based grouping: Cards can have multiple tags
    - Sub-whiteboards: Hierarchical organization of whiteboards
    Note: Cross-tag relation properties (e.g., link to cards with specific tags) not yet supported

task_support:
  native_tasks: yes  # Basic task support via Inbox app (To-do tab) since v1.58.0
  status_field: yes  # Via Kanban view with single-select property
  assignee_field: no  # Not natively supported; workaround via custom property
  due_date: no  # Basic tasks lack due dates; workaround via date property on tagged cards
  dependencies: workaround
  dependency_description: |
    No native dependency tracking. Workarounds include:
    - Using bi-directional links to reference blocking tasks
    - Visual connections (arrows) on whiteboards to show relationships
    - Custom properties to track blocking items (limited to text/select values)
    Official recommendation is to use dedicated task management tools (Todoist, ClickUp)
    for complex project management with dependencies.
  query_ready_tasks: no  # Cannot query "tasks with no blockers" - no dependency graph

query_capabilities:
  simple_filters: yes  # Property-based filtering: is, is not, contains, starts with, ends with, is empty
  graph_traversal: no  # Cannot traverse relationship chains programmatically
  multi_hop_queries: |
    Not supported. Cannot perform queries like "find all tasks blocked by tasks owned by X."
    Backlinks show direct connections but don't support multi-hop traversal.
    Future roadmap includes advanced search accounting for connections on whiteboards.
  query_language: none  # Visual filtering via UI only, no query DSL
  full_text_search: yes  # Sub-second search across tens of thousands of notes; includes OCR text from images
  vector_search: yes  # semantic_search_objects MCP tool uses semantic/meaning-based matching

scale:
  documented_limits: |
    - File upload: 2GB per file, no overall storage limit
    - Journal range query: ~3 months per MCP request
    - PDF search: Returns up to 80 ranked chunks
    - No documented limits on number of cards or whiteboards
  concurrent_access: |
    - Multi-device sync: Real-time sync across unlimited devices
    - Collaboration: Real-time editing with conflict prevention (same paragraph protection)
    - Data encrypted at rest and in transit (AWS storage)
  known_performance_issues: |
    - Local storage can fill up on desktop (data saved to C: drive by default)
    - Large PDF handling: AI avoids get_object on very large PDF cards
    - Mobile apps not fully feature-equal with desktop/web

hosting:
  hosted_option: yes
  hosted_pricing: |
    Pro: $8.99/month (yearly) or $11.99/month
    Premium: $17.99/month (yearly) - includes unlimited PDFs, AI models
  self_host_complexity: n/a  # No self-hosting option available
  self_host_requirements: n/a
  data_export: yes / markdown  # Export all notes as Markdown; JSON export via community MCP server

real_time:
  sync_mechanism: |
    Offline-first architecture with cloud sync:
    - Local data stored on device
    - Cloud sync via AWS (encrypted at rest and in transit)
    - Real-time cross-device syncing (launched October 2022)
  latency: Not documented; described as "real-time"
  conflict_resolution: |
    Multiple parties can edit concurrently with a real-time editing system
    that prevents merge errors if the same paragraph is edited concurrently.
    Specific CRDT/OT implementation not publicly documented.

agent_integration:
  mcp_tools_available: |
    Official MCP Server (9 tools):
    Read Operations:
    - semantic_search_objects: Keyword + semantic search across knowledge base
    - search_whiteboards: Find whiteboards by name/topic
    - get_whiteboard_with_objects: Full whiteboard structure and relationships
    - get_object: Read notes, journals, media cards, highlights, chat objects
    - get_journal_range: Retrieve journals in date range (up to ~3 months)
    - search_pdf_content: Keyword search in PDFs (up to 80 chunks)
    - get_pdf_pages: Retrieve specific PDF page ranges
    Write Operations:
    - save_to_note_card: Create new note cards in Inbox
    - append_to_journal: Add content to today's journal

    Community MCP Server (LarryStanley/heptabase-mcp) - works with backup data:
    - configureBackupPath, listBackups, loadBackup
    - searchWhiteboards, searchCards
    - getWhiteboard, getCard, getCardContent, getCardsByArea
    - exportWhiteboard (Markdown, JSON, HTML, Mermaid)
    - summarizeWhiteboard, analyzeGraph, compareBackups
  langchain_integration: no  # No official or community integration found
  llamaindex_integration: no  # No official or community integration found
  other_integrations: |
    - Claude Desktop: Native MCP support
    - Claude Code: Type /mcp and enter "heptabase-mcp" to authenticate
    - ChatGPT: MCP support (Personal Plus plan or higher, not Team plans)
    - Cursor: MCP compatible
    - Any MCP-compatible AI client
```

## Phase 2 Research Sources

- [Heptabase MCP Documentation](https://support.heptabase.com/en/articles/12679581-how-to-use-heptabase-mcp)
- [Heptabase Fundamental Elements](https://wiki.heptabase.com/fundamental-elements)
- [Heptabase Tag's Table & Kanban View](https://wiki.heptabase.com/tags-and-kanban)
- [Heptabase User Interface Logic](https://wiki.heptabase.com/user-interface-logic)
- [Heptabase 1.0 Announcement](https://wiki.heptabase.com/version-one)
- [Heptabase Collaboration](https://wiki.heptabase.com/collaborate-and-discuss-with-others)
- [Heptabase Roadmap](https://wiki.heptabase.com/roadmap/)
- [Heptabase Changelog](https://wiki.heptabase.com/changelog)
- [Community MCP Server - LarryStanley/heptabase-mcp](https://github.com/LarryStanley/heptabase-mcp)
- [Heptabase File Upload Limits](https://support.heptabase.com/en/articles/10447030-what-s-the-size-limit-for-uploading-files)
- [Heptabase Task App Location](https://support.heptabase.com/en/articles/11456351-where-is-the-task-app)
- [Heptabase Collaboration Q&A](https://support.heptabase.com/en/articles/10510497-collaboration-q-a)
- [NoteApps.info - Heptabase Review](https://noteapps.info/apps/heptabase)
