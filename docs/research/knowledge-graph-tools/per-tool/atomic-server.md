# Atomic-Server

```yaml
tool_name: Atomic-Server
category: Graph Database
official_url: https://atomicserver.eu/
pricing: open_source
pricing_details: |
  Completely free and open source under MIT license. No paid tiers.
  Self-hosted only - no managed cloud offering.
platforms:
  web_ui: yes
  desktop: yes  # Linux, Windows, macOS, ARM
  mobile: no  # Web UI accessible via mobile browser but no native apps
api:
  exists: yes
  type: REST  # Custom REST-like API with Atomic Data protocol
  read_write: both
open_source: yes
repo_url: https://github.com/atomicdata-dev/atomic-server
last_commit: 2026-01-30  # Very active development
github_stars: 1500  # Approximately 1.5k
screening_result: PASS
fail_reason:

additional_notes: |
  ## Overview
  Atomic-Server is a lightweight graph database / headless CMS written in Rust.
  It implements the "Atomic Data" specification - a typed, linked data format
  that's simpler than RDF while remaining compatible with it.

  ## Key Strengths for AI Agent Orchestration

  ### MCP Support (Native)
  - Built-in MCP (Model Context Protocol) support
  - Can use any model via OpenRouter or self-host with Ollama
  - This is a significant differentiator for AI agent integration

  ### Real-Time Capabilities
  - WebSocket support for live synchronization
  - SUBSCRIBE/UNSUBSCRIBE/GET/AUTHENTICATE commands
  - Server pushes COMMIT messages for subscribed resources
  - Enables collaborative, real-time applications

  ### API Details
  - REST-like API where every resource has a unique URL
  - Self-documenting: every Class, Property, Endpoint is itself a resource
  - Endpoints: /commits (writes), /query, /versions, /search
  - Content negotiation: JSON, JSON-AD, JSON-LD, Turtle, RDF/XML
  - Full-text search with <3ms response times (powered by tantivy)

  ### Performance
  - Sub-millisecond median response times
  - 8MB binary, no runtime dependencies
  - Event-sourced versioning via Atomic Commits

  ### Data Modeling
  - Built-in Ontology Editor for creating custom schemas
  - Strict schema validation
  - Type-safe graph database

  ### Rich UI Features
  - Airtable-like tables with keyboard support
  - Google Docs-like collaborative documents
  - Chat channels with attachments
  - File upload/download

  ## SDKs Available
  - JavaScript/TypeScript
  - React (with real-time hooks)
  - Svelte (with real-time stores)
  - Rust

  ## Considerations
  - Relatively young project compared to Neo4j/etc
  - Smaller community
  - Self-hosted only (no managed service)
  - Uses its own "Atomic Data" protocol (not standard GraphQL/SPARQL)

  ## Deployment
  - Docker: `docker run -p 80:80 -p 443:443 joepmeneer/atomic-server`
  - Cargo: `cargo install atomic-server`
  - Desktop installers available

additional_sections:
  mcp_integration:
    supported: yes
    details: |
      Native MCP support built into Atomic-Server.
      Can connect to OpenRouter for cloud models or Ollama for local models.
      This makes it uniquely positioned for AI agent workflows among graph databases.

  real_time_capabilities:
    websocket_api: yes
    protocol: AtomicData
    features:
      - Resource subscription
      - Live commit streaming
      - Authentication over WebSocket

  data_format:
    primary: Atomic Data (JSON-AD)
    compatible_with:
      - JSON
      - JSON-LD
      - RDF/XML
      - N-Triples
      - Turtle

  comparison_notes: |
    Atomic-Server sits in an interesting niche:
    - More developer-friendly than traditional triple stores
    - More structured than document databases
    - Has features of both a CMS and a graph database
    - Native AI/MCP integration is rare in this category
```

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://docs.atomicdata.dev/
  auth_model: cryptographic_keys  # Ed25519 public/private key pairs, signed requests, or Bearer tokens
  auth_details: |
    - Per-request signing with Ed25519 (most secure): x-atomic-public-key, x-atomic-signature, x-atomic-timestamp, x-atomic-agent headers
    - Bearer token authentication: base64-serialized Authentication Resource in Authorization header
    - WebSocket authentication: AUTHENTICATE {authenticationResource} message after connection
    - Authentication Resources contain: agent, publicKey, signature, timestamp, optional validUntil (default 30s)
    - No traditional API keys - uses cryptographic Agent identity model
  rate_limits: none_documented  # No rate limits mentioned in documentation
  webhooks: no  # Not mentioned in documentation
  subscriptions: yes  # WebSocket SUBSCRIBE/UNSUBSCRIBE for real-time resource updates
  mcp_server: native  # Built-in MCP support (not external server)
  mcp_server_url: n/a  # MCP is built into Atomic-Server itself, connects to OpenRouter or Ollama
  mcp_details: |
    - Native MCP (Model Context Protocol) support built into Atomic-Server
    - Can use any model via OpenRouter (cloud) or Ollama (self-hosted)
    - Not listed in awesome-mcp-servers as it's a native feature, not a separate MCP server
    - Unique among graph databases for having built-in AI assistant capabilities

data_model:
  node_structure: |
    - Every piece of data is a "Resource" with a unique URL (subject)
    - Resources have Properties (also URLs) with typed Values
    - Triple-based: Subject-Property-Value (similar to RDF but simpler)
    - JSON-AD is the default serialization (JSON where keys are Property URLs)
    - Self-documenting: Classes, Properties, and Endpoints are themselves Resources
  schema_flexibility: dynamic  # Ontology Editor allows creating custom Classes/Properties at runtime
  custom_fields: yes  # Create custom Properties and Classes via Ontology Editor
  relations: |
    - Relationships are modeled through Property values that are URLs pointing to other Resources
    - No edge properties directly - requires "reification" (creating a new Resource to describe the relationship)
    - This means relationships need their own Class definition to carry metadata
    - More structured than property graphs but requires more explicit modeling

task_support:
  native_tasks: no  # No built-in task/todo data type
  status_field: workaround  # Can be modeled via custom Properties
  assignee_field: workaround  # Can be modeled via custom Properties
  due_date: workaround  # Can be modeled via custom Properties
  dependencies: workaround  # Can be modeled via relationships between task Resources
  dependency_description: |
    - No native task or dependency features
    - Would need to create custom Classes: Task (with status, assignee, due_date Properties)
    - Dependencies modeled as relationships between Task Resources (e.g., "blockedBy" Property)
    - Flexible but requires manual schema design
  query_ready_tasks: workaround  # Would need to filter Collections by status and check blockedBy relationships

query_capabilities:
  simple_filters: yes  # Collections support filter by subject, property, value
  graph_traversal: yes  # Atomic Paths for traversing graphs by property
  multi_hop_queries: limited  # Paths can traverse multiple properties, but no Cypher-like pattern matching
  query_language: none  # No dedicated query language - uses RESTful HTTP + JSON-AD approach
  query_details: |
    - Collections: filter by property/value, sort by any property, pagination
    - Atomic Paths: traverse graphs by following property chains (URLs)
    - Query endpoint (/query): dynamic Collections without pre-defined Resource
    - Triple Pattern Fragments under the hood
    - No joins, no complex graph pattern matching
    - Example: can filter "all resources where status=pending" but not "all tasks blocked by tasks owned by user X" without multiple queries
  full_text_search: yes  # Powered by tantivy, typically <3ms response times, fuzzy search supported
  vector_search: no  # Not documented

scale:
  documented_limits: none_documented  # No explicit limits mentioned
  concurrent_access: |
    - WebSocket-based real-time sync supports multiple concurrent users
    - Multi-tenancy discussed as future feature (GitHub issue #288)
    - Current query performance may degrade with many non-public resources (per multi-tenancy discussion)
  known_performance_issues: |
    - Alpha status - "Breaking changes are expected until 1.0"
    - Multi-tenancy queries may slow down when filtering by access rights
    - No documented benchmarks for large-scale deployments

hosting:
  hosted_option: no  # Self-hosted only, no managed cloud offering
  hosted_pricing: n/a
  self_host_complexity: easy
  self_host_requirements: |
    - Docker: `docker run -p 80:80 -p 443:443 -v atomic-storage:/atomic-storage joepmeneer/atomic-server`
    - Or Cargo: `cargo install atomic-server` (requires build-essential, pkg-config, libssl-dev)
    - 8MB binary, no runtime dependencies
    - Built-in HTTPS via LetsEncrypt
    - Runs on Linux, Windows, macOS, ARM
    - Desktop installers available
  data_export: yes  # JSON, JSON-AD, RDF/XML, N-Triples, Turtle, JSON-LD formats

real_time:
  sync_mechanism: WebSocket  # wss connection to /ws endpoint
  protocol_details: |
    - Client commands: SUBSCRIBE, UNSUBSCRIBE, GET, AUTHENTICATE
    - Server messages: COMMIT (change data), RESOURCE (response to GET), ERROR
    - Subscribe to specific resource URLs for targeted updates
    - Silence on success, only ERROR messages on failure
  latency: sub_millisecond  # <1ms median response time, <3ms for full-text search
  conflict_resolution: not_documented  # No CRDT or OT mentioned, event-sourced via Atomic Commits

agent_integration:
  mcp_tools_available: |
    - Native AI assistant feature in Atomic-Server
    - Uses MCP to connect to LLMs (OpenRouter or Ollama)
    - Not a separate MCP server providing tools, but an MCP client consuming AI capabilities
    - Allows AI to interact with Atomic Data within the server
  langchain_integration: no  # No documented integration
  llamaindex_integration: no  # No documented integration
  other_integrations: |
    - SDKs: @tomic/lib (JS/TS), @tomic/react, @tomic/svelte, atomic_lib (Rust)
    - CLI: atomic-cli for terminal operations
    - All SDKs support real-time hooks/stores via WebSocket
    - React: single one-liner for real-time sync
    - No documented integrations with LangChain, LlamaIndex, or other AI frameworks
    - Custom integration would use REST API or SDKs

additional_notes: |
  ## Strengths for AI Agent Use Case

  1. **Native MCP Support**: Rare among databases - AI assistant built-in
  2. **Real-Time WebSocket**: Excellent for multi-agent coordination
  3. **Flexible Schema**: Ontology Editor makes it easy to define task structures
  4. **Fast Performance**: Sub-millisecond responses suitable for agent loops
  5. **Self-Documenting**: Schema is discoverable via API
  6. **Multiple Serialization**: Easy integration with various systems

  ## Limitations for AI Agent Use Case

  1. **No Native Task Model**: Must design custom schema for tasks/dependencies
  2. **Limited Query Power**: No graph pattern matching (like Cypher)
  3. **Multi-Hop Query Gaps**: Can't easily query "tasks blocked by X's tasks"
  4. **No Conflict Resolution**: May need external handling for concurrent edits
  5. **Alpha Status**: Breaking changes possible
  6. **No LangChain/LlamaIndex**: Custom integration work needed
  7. **Self-Hosted Only**: Operational overhead

  ## Recommendation

  Atomic-Server is promising for AI agent orchestration due to native MCP support
  and excellent real-time capabilities. However, the lack of powerful graph queries
  (no Cypher/SPARQL) and alpha status are concerns. Best suited for scenarios where:
  - Real-time sync between agents is critical
  - Schema flexibility is important
  - Team is comfortable with self-hosting
  - Query patterns are relatively simple (filtering, not complex traversals)
```

