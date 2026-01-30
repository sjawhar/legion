# LinkedDataHub

```yaml
tool_name: LinkedDataHub
category: Knowledge Graph Platform
official_url: https://linkeddatahub.com/
pricing: open_source
pricing_details: |
  Fully open-source under Apache 2.0 license. Free to self-host on local servers,
  enterprise clusters, or cloud environments using Docker. Also available as a
  free edition on AWS Marketplace. Commercial consulting, development, and support
  services available from AtomGraph on request.
platforms:
  web_ui: yes
  desktop: no
  mobile: no
api:
  exists: yes
  type: REST
  read_write: both
  details: |
    Implements uniform RESTful Linked Data API based on Linked Data Templates
    specification combined with SPARQL 1.1 Graph Store Protocol. Supports:
    - GET/POST/PUT/DELETE/PATCH for document management
    - SPARQL 1.1 Protocol endpoint at /sparql path
    - Content negotiation: Turtle, N-Triples, RDF/XML, JSON-LD, RDF/POST
    - Authentication: WebID-TLS (client certificates) or OpenID Connect (Google)
    - CLI wrapper scripts available for automation
open_source: yes
repo_url: https://github.com/AtomGraph/LinkedDataHub
last_commit: 2025-01-14
github_stars: 597
screening_result: PASS
fail_reason:

additional_notes: |
  LinkedDataHub is a low-code Knowledge Graph application platform developed by
  AtomGraph. It is specifically designed for RDF-based knowledge graphs and fully
  exploits the federated features of RDF and SPARQL.

  Key technical features:
  - Completely data-driven architecture where applications and documents are
    defined as data (no imperative code required for standard use cases)
  - Built-in SPARQL endpoint for each application
  - Access control system with fine-grained permissions
  - Import capabilities for CSV and RDF files
  - Transform pipelines for data processing
  - Docker-based deployment (requires 8GB RAM minimum)

  AI Agent Integration - Web-Algebra MCP Server:
  AtomGraph has developed Web-Algebra (github.com/AtomGraph/Web-Algebra), a
  companion project that provides Model Context Protocol (MCP) integration.
  This enables AI agents to:
  - Consume Linked Data and SPARQL through natural language instructions
  - Control and automate LinkedDataHub operations
  - Compile workflows into optimized JSON "bytecode" for atomic execution
  - 40+ operations covering SPARQL queries, Linked Data HTTP methods, URI
    manipulation, and LinkedDataHub-specific resource creation

  MCP deployment options include stdio transport, Streamable HTTP via FastAPI,
  and Claude Desktop integration. The Web-Algebra repo has 34 stars (as of
  Dec 2024) and requires Python + OpenAI API access.

  Target users: Researchers needing RDF-native FAIR data environments,
  developers building Knowledge Graph applications, and knowledge workers
  who need to model and manage graph data without technical details.

  Limitations:
  - SPARQL UPDATE queries not allowed via public endpoint (security measure)
  - Result limits may apply on queries
  - Authentication setup requires client certificates for full API access
  - No native mobile apps; web-based only
  - OpenID Connect (Google auth) currently UI-only, not supported via CLI/curl

additional_sections:
  mcp_integration:
    available: yes
    project_name: Web-Algebra
    repo_url: https://github.com/AtomGraph/Web-Algebra
    supported_clients:
      - Claude Desktop
      - OpenAI API-compatible agents
      - Generic MCP-compatible agents
    transport_modes:
      - stdio
      - Streamable HTTP (FastAPI/Uvicorn)
    capabilities:
      - Natural language to JSON DSL translation
      - 40+ RDF/SPARQL operations
      - Workflow compilation to atomic execution
      - LinkedDataHub-specific resource management

  rdf_standards_compliance:
    sparql_version: "1.1"
    graph_store_protocol: yes
    linked_data_templates: yes
    content_types:
      - Turtle
      - N-Triples
      - RDF/XML
      - JSON-LD
      - RDF/POST

  deployment_options:
    - Local server (Docker)
    - Enterprise cluster
    - AWS Marketplace (free edition)
    - Cloud environments

  related_projects:
    - name: LinkedDataHub-Apps
      url: https://github.com/AtomGraph/LinkedDataHub-Apps
      description: Demo and user-submitted data-driven applications
    - name: AtomGraph Core
      url: https://github.com/AtomGraph/Core
      description: Generic Linked Data framework for SPARQL triplestore backends
```

---

## Phase 2: Deep Evaluation

```yaml
# Phase 2: Deep Evaluation

api_details:
  documentation_url: https://atomgraph.github.io/LinkedDataHub/linkeddatahub/docs/reference/http-api/
  auth_model: other  # WebID-TLS (client certificates) or OpenID Connect (Google)
  rate_limits: |
    Configurable at nginx proxy layer. Internal requests are exempt from rate limiting.
    No specific limits documented; implementer-configurable.
  webhooks: no
  subscriptions: no  # No real-time subscriptions documented
  mcp_server: official  # Web-Algebra by AtomGraph
  mcp_server_url: https://github.com/AtomGraph/Web-Algebra

data_model:
  node_structure: |
    RDF-native graph store. Each document is stored in its own named graph.
    Data organized as triples (subject-predicate-object). Documents contain
    RDF resources described using any ontology/vocabulary. Navigate from
    document to graph using void:inDataset property. Backing triplestore
    is Apache Jena Fuseki by default.
  schema_flexibility: freeform  # Any RDF vocabulary can be used
  custom_fields: yes  # Any RDF property can be defined
  relations: |
    RDF triples naturally express relations. Any resource can link to any
    other via URI references. Supports owl:sameAs, rdfs:seeAlso, and
    arbitrary domain-specific predicates. Federated queries can traverse
    across external SPARQL endpoints.

task_support:
  native_tasks: no  # No built-in task model; tasks must be modeled as RDF
  status_field: no  # Would need custom ontology
  assignee_field: no  # Would need custom ontology (e.g., foaf:Agent)
  due_date: no  # Would need custom ontology (e.g., xsd:dateTime property)
  dependencies: workaround
  dependency_description: |
    No native task dependencies. However, RDF allows modeling any dependency
    structure using custom predicates (e.g., :blockedBy, :dependsOn). SPARQL
    property paths enable multi-hop traversal of dependency chains. You would
    need to define your own task ontology with status, assignee, due date,
    and dependency properties.
  query_ready_tasks: workaround  # Via SPARQL query with custom task ontology

query_capabilities:
  simple_filters: yes  # SPARQL FILTER clauses
  graph_traversal: yes  # Full SPARQL 1.1 property paths support
  multi_hop_queries: |
    Supported via SPARQL 1.1 property paths. Example: "find all tasks blocked
    by tasks owned by X" would use patterns like:
    ?task :blockedBy+/:assignee :personX
    Property paths support *, +, ?, sequence (/), alternatives (|).
  query_language: sparql  # SPARQL 1.1 (SELECT, CONSTRUCT, DESCRIBE, ASK)
  full_text_search: no  # Would depend on triplestore capabilities
  vector_search: no

scale:
  documented_limits: |
    No specific limits documented in LinkedDataHub itself. Result limits
    may apply on SPARQL queries. Request payload limits enforced (413 errors).
    Backing triplestore (Jena Fuseki) suitable for "personal dataspaces to
    moderately-sized enterprise Knowledge Graphs."
  concurrent_access: |
    Multi-user access via W3C ACL-based access control. Agents and groups
    can be granted Read/Write/Append/Control modes on documents or resource
    classes. Three default groups: Owners, Writers, Readers. Public access
    can be enabled for unauthenticated users.
  known_performance_issues: |
    SPARQL UPDATE not allowed on public endpoint (security measure).
    Backend response caching via Varnish helps with read performance.

hosting:
  hosted_option: yes  # Cloud version available; AWS Marketplace free edition
  hosted_pricing: |
    Free edition on AWS Marketplace. Commercial consulting and support
    services available from AtomGraph on request.
  self_host_complexity: moderate
  self_host_requirements: |
    Docker and Docker Compose required. bash shell, OpenSSL on PATH.
    No specific memory/CPU requirements documented officially, though
    8GB RAM mentioned in Phase 1 notes. Jena CLI tools needed for CLI scripts.
    SSL certificate setup required for WebID-TLS authentication.
  data_export: yes  # RDF/XML, Turtle, N-Triples, JSON-LD, CSV via SPARQL CONSTRUCT

real_time:
  sync_mechanism: |
    No WebSocket or real-time push mechanism. ETag-based caching with
    content hashes. Backend response caching via Varnish HTTP proxy.
    Polling-based approach would be required for real-time updates.
  latency: |
    Not documented. ETag headers enable conditional requests.
    Age header shows cache age in seconds.
  conflict_resolution: |
    Not documented for concurrent edits. HTTP-based optimistic locking
    possible via ETag/If-Match headers. No CRDT or operational transform.

agent_integration:
  mcp_tools_available: |
    Via Web-Algebra MCP server:
    - Linked Data: GET, PATCH, POST, PUT
    - SPARQL: CONSTRUCT, DESCRIBE, SELECT, Substitute
    - URI/String: ResolveURI, EncodeForURI, Concat, Replace, Str, URI
    - Control Flow: Value, Variable, ForEach
    - LinkedDataHub-specific: ldh-CreateContainer, ldh-CreateItem, ldh-List,
      ldh-AddGenericService, ldh-AddResultSetChart, ldh-AddSelect,
      ldh-AddView, ldh-AddObjectBlock, ldh-AddXHTMLBlock, ldh-RemoveBlock
    Total: 40+ operations
  langchain_integration: no  # No official integration, but langchain-rdf package exists for generic RDF/SPARQL
  llamaindex_integration: no  # No official integration
  other_integrations: |
    - Claude Desktop (via MCP)
    - OpenAI API-compatible agents (via Web-Algebra)
    - Jena CLI tools for scripting/automation
    - Generic SPARQL 1.1 clients
    - Any Linked Data client supporting content negotiation
```
