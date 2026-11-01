# Open Questions: Hands-On Testing Required

These questions emerged from Phase 2 research and cannot be answered without hands-on experimentation.

---

## Priority 1: Critical Path Questions

### Q1: Can Fibery's GraphQL efficiently query "ready tasks"?

**Context:** Fibery has native dependencies and an `isEmpty` filter for relations. In theory, you can query tasks where `blockedBy.isEmpty() == true`.

**Test:**
1. Create 100 tasks with various dependency chains
2. Query tasks with no incomplete blockers via GraphQL
3. Measure response time and verify correctness

**Success Criteria:** <500ms response, correct filtering of blocked tasks

---

### Q2: Neo4j CDC vs Polling for Agent Notifications

**Context:** Neo4j doesn't have native WebSocket subscriptions. Options are:
- CDC (Change Data Capture) + Kafka
- Polling with `lastModified` timestamps
- Third-party tools

**Test:**
1. Set up Neo4j with CDC enabled
2. Compare latency: CDC+Kafka vs 1-second polling
3. Measure resource overhead of each approach

**Success Criteria:** Determine if CDC is worth the complexity vs simple polling

---

### Q3: Dgraph Subscription Performance Under Load

**Context:** Dgraph has native GraphQL subscriptions, which is rare among graph databases. But how do they perform with multiple concurrent subscribers?

**Test:**
1. Create 10 subscription connections (simulating 10 agents)
2. Perform 100 mutations that trigger subscription updates
3. Measure delivery latency and any dropped notifications

**Success Criteria:** <100ms notification latency, zero dropped updates

---

### Q4: Beads Scale Ceiling

**Context:** Documentation suggests performance degrades at ~200 active tasks. Need to verify this and understand the failure mode.

**Test:**
1. Create 500 tasks with complex dependency chains
2. Run `bd ready` repeatedly
3. Monitor response time and memory usage

**Success Criteria:** Identify actual performance ceiling and failure mode

---

## Priority 2: Architecture Decisions

### Q5: Two-Layer vs Single-Layer Architecture

**Context:** Recommendation is Fibery (tasks) + Neo4j (knowledge). But is the complexity worth it?

**Test:**
1. Prototype single-layer with Neo4j only (model tasks as nodes)
2. Prototype two-layer with Fibery + Neo4j
3. Compare: development complexity, query performance, operational overhead

**Success Criteria:** Quantify trade-offs to make informed architecture decision

---

### Q6: AFFiNE CRDT Behavior with Many Agents

**Context:** AFFiNE uses CRDT for conflict-free edits, but what happens with 10+ concurrent agents?

**Test:**
1. Simulate 10 agents making concurrent edits to the same workspace
2. Measure sync latency and conflict resolution correctness
3. Monitor memory/CPU overhead

**Success Criteria:** Understand CRDT limits for multi-agent scenarios

---

### Q7: Graphiti as Memory Layer + External Task Manager

**Context:** Graphiti excels at agent memory but has no task support. Can it complement Linear/Fibery effectively?

**Test:**
1. Set up Graphiti for knowledge/context storage
2. Set up Linear for task management
3. Build integration that links tasks to knowledge entities
4. Evaluate workflow friction

**Success Criteria:** Clean separation of concerns, acceptable integration overhead

---

## Priority 3: Edge Cases

### Q8: Rate Limit Impact on Agent Workflows

**Context:** Several tools have strict rate limits (Fibery 3/sec, Linear 5000/hr, Coda 10/6sec).

**Test:**
1. Simulate realistic agent workflow (create task, update status, query dependencies)
2. Measure how quickly rate limits are hit
3. Test retry/backoff strategies

**Success Criteria:** Understand sustainable request patterns per tool

---

### Q9: MCP Server Reliability Under Load

**Context:** Official MCP servers exist for Neo4j, Fibery, Linear, Graphiti, but their reliability under agent load is unknown.

**Test:**
1. Send 100 concurrent MCP requests to each server
2. Measure success rate, latency distribution, error types
3. Test recovery from transient failures

**Success Criteria:** >99% success rate, <500ms P95 latency

---

### Q10: Data Migration Path Between Tools

**Context:** If we start with one tool and need to migrate, how painful is it?

**Test:**
1. Export data from Fibery (tasks + relations)
2. Import into Neo4j (as graph)
3. Verify data integrity and relationship preservation

**Success Criteria:** Lossless migration with documented transformation steps

---

## Testing Priority Matrix

| Question | Impact | Effort | Priority |
|----------|--------|--------|----------|
| Q1: Fibery ready-task query | High | Low | **P0** |
| Q2: Neo4j CDC vs polling | High | Medium | **P0** |
| Q3: Dgraph subscriptions | Medium | Medium | P1 |
| Q4: Beads scale ceiling | Medium | Low | P1 |
| Q5: Two-layer architecture | High | High | P1 |
| Q6: AFFiNE CRDT | Low | Medium | P2 |
| Q7: Graphiti + Linear | Medium | Medium | P2 |
| Q8: Rate limits | Medium | Low | P1 |
| Q9: MCP reliability | Medium | Medium | P1 |
| Q10: Data migration | Low | Medium | P2 |

---

## Recommended Testing Order

1. **Week 1:** Q1 (Fibery queries) + Q4 (Beads scale) — quick validation of top task tools
2. **Week 2:** Q2 (Neo4j CDC) + Q3 (Dgraph subscriptions) — real-time architecture decision
3. **Week 3:** Q5 (Architecture comparison) — build minimal prototypes
4. **Week 4:** Q8 (Rate limits) + Q9 (MCP reliability) — operational readiness
