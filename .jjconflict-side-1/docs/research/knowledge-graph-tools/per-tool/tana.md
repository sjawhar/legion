# Tana - Phase 1 Screening

```yaml
tool_name: Tana
category: PKM / Knowledge Graph
official_url: https://tana.inc
pricing: freemium
pricing_details: |
  Free: 500 AI credits/month, core editor features with limits
  Plus: $10/month ($8/month annual) - 2,000 AI credits, integrations, workspace sharing
  Pro: $18/month ($14/month annual) - 5,000 AI credits, unlimited workspaces, advanced features
  Student/NGO discounts available
platforms:
  web_ui: yes
  desktop: yes  # Mac, Windows, Linux - with offline support
  mobile: both  # iOS (Nov 2024), Android (Feb 2025)
api:
  exists: yes
  type: REST
  read_write: write_only  # CONFIRMED: Read API is on roadmap but not yet available
open_source: no  # Core product is closed-source
repo_url: https://github.com/tanainc  # Organization with tools/samples, not core product
last_commit: null  # N/A - closed source product
github_stars: null  # N/A - closed source product
screening_result: FAIL
fail_reason: API is write-only - no read capabilities available. Read API is on roadmap but not yet implemented.

additional_notes: |
  ## API Details (Input API)
  - CONFIRMED: Write-only API (called "Input API")
  - Endpoint: https://europe-west1-tagr-prod.cloudfunctions.net/addToNodeV2
  - Authentication: Per-workspace API tokens generated in Tana client
  - Rate limits: 1 request/second per token
  - Payload limits: 5,000 characters max, 100 nodes per request
  - Workspace limit: Won't sync beyond 750k nodes

  ## What the API CAN do (write operations):
  - Create nodes (plain text, URLs, dates, references, checkboxes, files)
  - Apply supertags to nodes
  - Create fields and structured data
  - Update node names (limited)
  - Upload files (base64 encoded)

  ## What the API CANNOT do:
  - Read/query existing nodes
  - Search the knowledge graph
  - Retrieve node content or metadata
  - List nodes or supertags

  ## MCP Server Available
  - Community MCP server: https://github.com/tim-mcdonnell/tana-mcp (38 stars)
  - MIT licensed, works with Claude Desktop, Cursor, Raycast
  - Limited by underlying write-only API - cannot read from Tana
  - Author explicitly notes: "Until there's a Tana API that can actually read and interact
    with a whole Tana knowledge graph, we will be unable to have a true Tana MCP server"

  ## Product Strengths (if API had read access)
  - Powerful knowledge graph with outliner UX
  - Strong AI integration (voice, chat, meeting notes)
  - Supertags system for structured data
  - Desktop offline support
  - Active development (Feb 2025: $25M Series A at $100M valuation)
  - 160K+ user waitlist during beta

  ## Agent Orchestration Implications
  - CANNOT use Tana as a knowledge retrieval source for agents
  - CAN use Tana as a destination for agent outputs (write-only)
  - Would require workarounds (export/sync) to read Tana data
  - Not suitable for bidirectional agent-knowledge graph workflows

additional_sections:
  ai_features:
    description: Built-in AI capabilities
    details:
      - AI chat with context from your graph
      - Voice transcription and voice chat
      - Meeting agent for automated notes
      - AI credits system (varies by plan)
      - Model selection available on Pro plan

  real_time_capabilities:
    description: Collaboration and sync features
    details:
      - Real-time workspace sharing (Plus/Pro plans)
      - Desktop offline mode with full graph access
      - Mobile offline capture (but not offline reading)

  future_roadmap:
    description: Planned features relevant to this evaluation
    details:
      - Read API explicitly on roadmap (no timeline given)
      - Search API mentioned in community discussions
      - Would dramatically change screening result if implemented
```
