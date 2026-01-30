# RemNote - Phase 1 Screening

```yaml
tool_name: RemNote
category: PKM / Knowledge Graph
official_url: https://www.remnote.com/
pricing: freemium
pricing_details: |
  Free: Unlimited notes and flashcards, unlimited synced devices, 3 annotated PDFs, 5 image occlusion cards
  Pro: $8/month (billed annually) or $10/month - unlimited PDF annotation, image occlusion, tables, templates, unlimited file uploads, AI features
  Pro+AI: Full learning suite with 20,000 AI credits for advanced AI learning
  Life-Long Learner: $300-395 one-time payment for 5 years of Pro features
  Education discount: 25% off for students, faculty, and staff
platforms:
  web_ui: yes
  desktop: Windows, Mac, Linux
  mobile: both  # iOS and Android
api:
  exists: no  # No external REST API - only internal Plugin SDK
  type: Plugin SDK (JavaScript/React)
  read_write: both  # Plugin SDK supports full CRUD within RemNote
open_source: no  # Core app is closed source
repo_url: https://github.com/remnoteio  # Organization with plugin templates and official plugins
last_commit: 2026-01  # Plugin templates actively maintained
github_stars: ~31  # remnote-plugin-template-react has 31 stars
screening_result: FAIL
fail_reason: |
  No external API. RemNote only offers a Plugin SDK that runs within the RemNote
  application itself. There is no REST API, GraphQL API, or other programmatic
  external access method. The backend API was deprecated and they explicitly state
  "We currently don't host a backend API." All integrations must be built as
  frontend plugins that run inside RemNote, not external services that connect to it.

additional_notes: |
  RemNote is a powerful knowledge management tool combining note-taking with spaced
  repetition learning. Founded in 2018 by students frustrated with switching between
  apps, it's based in Berlin and has raised $2.8M in seed funding.

  Key features:
  - Bi-directional linking (like Obsidian/Roam)
  - Knowledge graph visualization
  - Spaced repetition flashcards built-in
  - PDF annotation
  - AI-powered features (summarization, question generation)
  - Offline mode support

  Plugin SDK capabilities (but NOT external API):
  - Full CRUD operations on "Rem" (their term for notes/blocks)
  - Create/read/update/delete Rem
  - Generate and manage flashcards
  - Inject UI widgets throughout RemNote
  - Access to editor and queue components
  - Rate limited: ~1000 Rem creation takes ~25 seconds

  The Plugin SDK is React-first with TypeScript support, but plugins run inside
  RemNote's iframe sandbox or main thread - they cannot be called externally.

  Third-party integrations exist through:
  - Chrome Extension (RemNote Clipper)
  - Twitter bot (RemNoteBot)
  - Telegram bot (RemNote-bot)
  - IFTTT integrations
  - Readwise (rebuilt as frontend plugin after backend API deprecation)

  For AI agent orchestration, the lack of external API is a significant limitation.
  An agent cannot programmatically read/write RemNote data without running as a
  plugin inside the RemNote application itself. This makes it unsuitable for
  server-side agent workflows or automated knowledge base management.

additional_sections:
  learning_focus: |
    RemNote differentiates itself through deep integration of spaced repetition
    learning techniques. It's optimized for students and lifelong learners rather
    than general-purpose knowledge management.

  plugin_ecosystem: |
    Active plugin ecosystem with official templates:
    - remnote-plugin-template-react (31 stars)
    - remnote-official-plugins (30 stars)
    - remnote-theme-template
    - FSRS scheduler (now integrated into core)
    Community has created Citation Manager, Zotero integration, and more.

  mcp_availability: |
    No MCP server found. Given the lack of external API, an MCP server would
    need to somehow wrap the Plugin SDK or use unofficial methods.

  real_time_capabilities: |
    Syncs across devices (web, desktop, mobile). Offline mode available.
    No webhook or real-time event system for external integrations.
```
