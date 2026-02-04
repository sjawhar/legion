# OpenCode and AI Coding Agent Workflow Orchestration Research

**Date**: 2026-02-06
**Focus**: Multi-agent coordination patterns, workflow orchestration, and cross-model review strategies

## Executive Summary

OpenCode represents a different philosophy than Claude Code: it's built for **multi-model flexibility** and **explicit orchestration** rather than Claude Code's "polished, opinionated" approach. Several community-built orchestration layers (Oh My OpenCode, OpenAgentsControl, opencode-background-agents) demonstrate mature patterns for multi-agent coordination that Legion could adopt or learn from.

**Key Finding for Legion**: OpenCode can be invoked programmatically via CLI, SDK, and server API, making it feasible for cross-model-family review integration. The community has already built sophisticated multi-agent orchestration patterns that align with Legion's architecture.

## 1. OpenCode Core Architecture

### Design Philosophy

- **Model Agnostic**: Supports 75+ LLM providers (OpenAI, Anthropic, Google, Azure, Groq, local models via Ollama)
- **Explicit Agent Loop**: Clear separation of planning → execution → verification
- **Client/Server Architecture**: Enables remote execution, Docker containers, persistent workspaces
- **Plan-First**: Agent builds plan before execution, making behavior predictable
- **No Hidden Abstractions**: Uses filesystem and shell directly

### Programmatic Invocation (Three Methods)

#### 1. CLI Non-Interactive Mode
```bash
opencode -p "prompt" -f json  # Auto-approves all permissions
```
- Direct command-line invocation
- Useful for scripting and automation
- Returns structured output with `-f json`

#### 2. Server API
```bash
opencode serve  # Starts server on localhost:4096
```
- OpenAPI 3.1 specification at `http://localhost:4096/doc`
- RESTful API for programmatic control
- Swagger explorer available
- Multiple clients can connect

#### 3. JavaScript/TypeScript SDK
```typescript
import { OpencodeClient } from '@opencode/sdk'
// Type-safe client with TypeScript definitions
// Generated from OpenAPI spec
```

**Feasibility for Legion**: High. OpenCode can be integrated as a reviewBot subprocess that:
1. Receives PR context via CLI arguments
2. Runs review with different model (e.g., GPT-4o, DeepSeek)
3. Returns structured feedback via JSON output

### Model Provider Integration

**OpenCode Zen Gateway**: Curated model/provider combinations that have been benchmarked and verified to work well. Solves the "which model/provider combo actually works?" problem.

**Variant System**: Define multiple configurations for the same model without duplication. Quick switching via `variant_cycle` keybind.

**Priority Order**:
1. `--model` or `-m` CLI flag
2. Config file setting
3. Default model

## 2. Oh My OpenCode - Orchestration Layer

**GitHub**: https://github.com/code-yeongyu/oh-my-opencode
**Website**: https://ohmyopencode.com/

### Architecture

**Core Concept**: Batteries-included orchestration layer that wraps OpenCode with opinionated agents, hooks, MCPs, and configuration for reliable multi-agent workflows.

### The Sisyphus Agent System

**Sisyphus** (main orchestrator) leads a team of specialized agents:

| Agent | Model | Role |
|-------|-------|------|
| Sisyphus | Claude 3.5 Sonnet | Main orchestrator, task delegation |
| Librarian | Claude 3.5 Sonnet | Documentation analysis, project history |
| Explorer | Grok Code | Codebase navigation at speed |
| Oracle | GPT-5.2 Medium | Strategy, design, debugging |
| Frontend/UIUX | Gemini 3 Pro | UI code, visual elements |

**Pattern**: Sisyphus analyzes user command → delegates to most suitable sub-agent → integrates results.

### Agent Orchestration Patterns

#### Two-Mode Agent System

**Primary Agents**:
- Accept UI-selected models
- Full delegation rights
- Entry points for user requests
- Unrestricted delegation capabilities

**Subagent Agents**:
- Cannot accept UI model overrides
- Restricted tool access (prevents capability escalation)
- Execute or advise on specific tasks
- Can be read-only or specialized

#### Three-Tier Hierarchy

```
Primary (Orchestrator)
  ↓ delegates to
Advisory/Planning (Strategist)
  ↓ delegates to
Execution/Research (Specialist)
```

**Prevents**: Recursive delegation loops, capability escalation

#### Delegation Patterns

Two methods:
1. `delegate_task` - Flexible routing, orchestrator decides which agent
2. `call_omo_agent` - Direct call to specific agent

**Tool Restrictions**: Enforced via permission objects to prevent escalation (e.g., review agents get read-only + docs, no bash)

#### 6-Phase Workflow

```
UNDERSTAND → PLAN → DELEGATE → INTEGRATE → VERIFY → DELIVER
```

**Key Insight**: Independent tasks should run simultaneously.
- **Parallel execution**: Put all subagent calls in single message
- **Sequential execution**: Separate messages for dependent tasks

### Features

- 10 specialized agents
- 32 lifecycle hooks
- 20+ tools including background task execution
- Understands complex repo structures, build systems
- Handles Hugo/React hybrids, Vite configs, custom build scripts
- Context-management hooks prevent token blow-ups

**Legion Relevance**: The three-tier hierarchy and two-mode system are directly applicable to Legion's controller → worker → subprocess pattern.

## 3. OpenAgentsControl - Plan-First Framework

**GitHub**: https://github.com/darrenhinde/OpenAgentsControl

### Core Workflow

**Approval-Based Execution**: Agents ALWAYS request approval before execution
```
Propose → Approve → Execute
```

### 6-Stage Process

```
Analyze → Approve → Execute → Validate → Summarize → Confirm
```

**Automatic delegation** to specialized subagents when needed.

### Design Principles

**Minimal Viable Information (MVI)**:
- Only load what's needed when needed
- Context files under 200 lines
- Lazy loading for faster responses
- Token efficiency

**Team-Ready Patterns**:
- Store coding patterns once, entire team uses same standards
- Context committed to repository
- New developers inherit team patterns automatically

### Core Agents

1. **openagent** - Universal coordinator for general tasks
2. **opencoder** - Specialized development agent for complex coding
3. **system-builder** - Meta-level generator for custom AI architectures

**Multi-language Support**: TypeScript, Python, Go, Rust with automatic testing, code review, validation

**Legion Relevance**: The approval-based workflow could inform Legion's "user-input-needed" label behavior. The MVI principle is critical for token efficiency with many parallel workers.

## 4. OpenCode Background Agents

**GitHub**: https://github.com/kdcokenny/opencode-background-agents

### Architecture

**Claude Code-style background agents** with async delegation and context persistence.

### Key Features

**Async Delegation**:
```
1. Delegate task: "Research OAuth2 PKCE best practices"
2. Continue coding/brainstorming
3. Receive system reminder when done
```

**Context Persistence**:
- Results persisted to `~/.local/share/opencode/delegations/`
- Markdown files with title and summary
- AI can scan past research and find relevant context
- Persistence layer ensures results are discoverable

**Installation**: Via OCX (package manager for OpenCode extensions)

**Legion Relevance**: This is essentially what Legion's backlog-worker pattern does, but for research tasks. The persistence to markdown with metadata is similar to Legion's retro phase documentation.

## 5. Multi-Agent Code Review Systems

### Pattern: Multiple Specialized Reviewers

**Architecture**: Analyze diffs from multiple expert perspectives simultaneously, invoke only needed specialists.

**Example**: Single PR touches React + API + K8s + GitHub Actions → deploy 4 specialized reviewers:
1. Frontend reviewer (accessibility issues)
2. Backend reviewer (N+1 queries)
3. DevOps reviewer (health check concerns)
4. Security reviewer (auth/authz flaws)

### Specialized Reviewer Roles

#### Security Reviewer
- Input validation/sanitization
- Authentication/authorization flaws
- SQL injection, XSS vulnerabilities
- Secrets/sensitive data exposure
- OWASP Top 10 compliance

#### Performance Reviewer
- Database query efficiency
- N+1 problems
- Memory leaks
- Algorithm complexity
- Caching strategies
- Bundle size

#### Quality Reviewer
- Code style consistency
- Test coverage
- Documentation completeness
- Error handling patterns

### Parallel Execution

Deploy specialized sub-agents in parallel for:
- Faster analysis (concurrent execution)
- Deeper analysis (multiple perspectives)
- Confidence scoring (aggregate opinions)

### Synthesis and Reporting

**Duplicate Handling**:
- Keep finding with highest severity
- Merge context from all agents
- Note which perspectives flagged it

**Comprehensive Scoring**: 0-100 scores for security, quality, performance, tests to quantify code health

**Standardized Output**: Approval/rejection with actionable recommendations

**Legion Relevance**: Cross-model-family review could use this pattern:
- Claude Code worker implements feature
- OpenCode reviewers (GPT-4o, DeepSeek, Gemini) analyze in parallel
- Synthesize findings with confidence scores
- Post consolidated review to PR

## 6. Aider - Architect/Editor Pattern

**Website**: https://aider.chat/

### Architect Mode

**Two-Model Coordination**: Split reasoning and editing into separate inference steps

```
Architect Model → Solve coding problem, propose solution
     ↓
Editor Model → Translate solution into specific file edits
```

**Benefits**:
- Architect focuses on solving problem naturally (no edit format constraints)
- Editor focuses on generating well-formed code edits
- Better results than single-model approach for many models

### Multi-File Coordination

**Capabilities**:
- Codebase mapping (understands entire project structure)
- Multi-file editing (refactor across files)
- Automatic commit messages
- Supports 100+ languages
- Git-heavy workflows with frequent refactoring

**Practical Workflow**:
1. Start planning in architect mode
2. Switch to code mode to build
3. Use ask mode to check understanding

### Model Combinations (Performance)

**SOTA**: 85% score using o1-preview (Architect) + DeepSeek/o1-mini (Editor)

**Self-pairing works well**: Sonnet, GPT-4o, GPT-4o-mini scored higher when used as Architect/Editor pair

**Legion Relevance**: Legion's backlog phase (architect worker) could adopt this pattern - use planning model to generate spec, then implementation model to execute.

## 7. Continue.dev - Multi-Mode Agent

**Website**: https://continue.dev/
**GitHub**: https://github.com/continuedev/continue

### Architecture

**IDE Integration**: VS Code and JetBrains as native extensions (not terminal-based)

### Three Interaction Modes

1. **Chat Mode** - Conversational assistance
2. **Plan Mode** - Multi-step planning
3. **Agent Mode** - Autonomous multi-file operations

### Workflow Orchestration

**Pre-configured Templates**: Common tasks with configurable triggers
- Cron-based scheduling
- Webhook triggers
- Specific agents
- Target repositories

**CI/CD Integration**: GitHub Actions, Jenkins, GitLab CI

**Continue CLI**: Command-line agent for:
- Understanding codebase
- Making intelligent decisions
- Executing complex workflows
- Building async coding agents

### Model Context Protocol

**MCP Tools Integration**: GitHub, Sentry, Snyk, Linear

**Configuration as Code**: `.continue/rules/` directory defines:
- Team standards
- Coding patterns
- AI behaviors
- Ensures AI suggestions comply with guidelines

**Legion Relevance**: The MCP integration and configuration-as-code patterns are directly applicable. Legion already uses Linear MCP; could extend with Sentry for error tracking.

## 8. Architecture Comparison: Claude Code vs OpenCode vs Aider

### Claude Code

**Philosophy**: "Apple approach" - polished, opinionated, ecosystem lock-in

**Strengths**:
- Repository mapping within seconds
- Automated workflows (triage, refactoring, testing, PR)
- Single-command actions
- Contextual reasoning and multi-file understanding
- Claude 3.5 Sonnet, Claude 3 Opus support

**Limitations**:
- Locked to Anthropic ecosystem
- Can't swap models mid-session
- Simpler CLI design (no persistent workspaces)

**Performance**: Built for speed

### OpenCode

**Philosophy**: "Linux approach" - flexibility, multi-model, explicit control

**Strengths**:
- 75+ LLM providers (vendor-agnostic)
- Switch models mid-session with context preservation
- Client/server design enables remote execution
- Persistent workspaces (even when laptop closed)
- Predictable behavior (no hidden abstractions)
- Explicit agent loop: planning → execution → verification

**Limitations**:
- Less polished out-of-box experience
- Requires more configuration

**Performance**: Built for thoroughness

### Aider

**Philosophy**: Git-first, terminal-native pair programmer

**Strengths**:
- Automatic commit messages
- Multi-file coordination
- Git-heavy workflows
- Codebase mapping
- Works with Claude 3.7 Sonnet, DeepSeek R1/V3, OpenAI o1/o3-mini/GPT-4o
- Architect/Editor pattern for reasoning separation

**Use Case**: Frequent refactoring with Git integration

## 9. Cross-Model Review Strategy for Legion

### Proposed Architecture

```
Legion Worker (Claude Code)
   ↓ implements feature, opens PR
OpenCode Review Bot (via legion review skill)
   ↓ programmatic invocation
Parallel Review Agents (different models)
   ├─→ GPT-4o (general code review)
   ├─→ DeepSeek (performance analysis)
   └─→ Gemini (security analysis)
   ↓ parallel execution
Synthesizer
   ↓ deduplicates, scores, consolidates
PR Comment (structured feedback)
```

### Implementation Approach

#### 1. OpenCode CLI Invocation
```bash
opencode -p "Review PR #123 focusing on security" \
  --model anthropic/claude-3-5-sonnet \
  -f json
```

#### 2. Model Variants Configuration
```yaml
# .opencode/config.yaml
models:
  security-reviewer:
    provider: google
    model: gemini-3-pro
    temperature: 0.1
    tools: [read, grep, web_search]

  performance-reviewer:
    provider: deepseek
    model: deepseek-chat-v3
    temperature: 0.2
    tools: [read, grep, bash]

  general-reviewer:
    provider: openai
    model: gpt-4o
    temperature: 0.3
    tools: [read, grep, web_search]
```

#### 3. Parallel Execution Pattern
```python
import asyncio
from legion import opencode_integration

async def review_pr(pr_number: int):
    # Launch reviewers in parallel (Oh My OpenCode pattern)
    tasks = [
        opencode_integration.review(
            model="security-reviewer",
            prompt=f"Review PR {pr_number} for security issues"
        ),
        opencode_integration.review(
            model="performance-reviewer",
            prompt=f"Review PR {pr_number} for performance issues"
        ),
        opencode_integration.review(
            model="general-reviewer",
            prompt=f"Review PR {pr_number} for code quality"
        )
    ]

    results = await asyncio.gather(*tasks)
    return synthesize_reviews(results)
```

#### 4. Synthesis and Deduplication
```python
def synthesize_reviews(results: list[ReviewResult]) -> ConsolidatedReview:
    """
    Following multi-agent review patterns:
    - Deduplicate findings (keep highest severity)
    - Merge context from all reviewers
    - Generate confidence scores
    - Produce actionable recommendations
    """
    findings = defaultdict(list)

    for result in results:
        for finding in result.findings:
            findings[finding.issue_id].append({
                'reviewer': result.model,
                'severity': finding.severity,
                'context': finding.context
            })

    consolidated = []
    for issue_id, reviews in findings.items():
        consolidated.append({
            'issue_id': issue_id,
            'severity': max(r['severity'] for r in reviews),
            'reviewers': [r['reviewer'] for r in reviews],
            'confidence': len(reviews) / len(results),  # % agreement
            'context': merge_contexts([r['context'] for r in reviews])
        })

    return ConsolidatedReview(
        findings=consolidated,
        scores={
            'security': calculate_score(consolidated, 'security'),
            'performance': calculate_score(consolidated, 'performance'),
            'quality': calculate_score(consolidated, 'quality')
        }
    )
```

### Integration Points

**When to Trigger**:
- After Legion worker opens PR (needs-review state)
- Before human review (pre-screening)
- On-demand via comment trigger (e.g., `/review-cross-model`)

**Where Results Go**:
- Post as PR comment with structured feedback
- Update Linear issue with review summary
- Set PR labels based on severity findings

## 10. Workflow Orchestration Patterns Summary

### Key Patterns Identified

#### 1. Agent Hierarchy (Oh My OpenCode)
```
Primary (Orchestrator) - Full delegation, all tools
  ↓
Advisory (Strategist) - Restricted delegation, planning tools
  ↓
Execution (Specialist) - No delegation, task-specific tools
```

**Prevents**: Recursive loops, capability escalation
**Enables**: Clear separation of concerns

#### 2. Approval-Based Workflow (OpenAgentsControl)
```
Analyze → Approve → Execute → Validate → Summarize → Confirm
```

**Key**: Human approval before execution
**Benefit**: Safety, transparency, auditability

#### 3. Architect/Editor Split (Aider)
```
Architect (Reasoning) → Propose solution
Editor (Execution) → Generate code edits
```

**Key**: Separate reasoning from implementation
**Benefit**: Better results, model specialization

#### 4. Parallel Execution (Multi-Agent Review)
```
Task → [Agent1, Agent2, Agent3] → Synthesizer → Result
```

**Key**: Independent tasks run concurrently
**Benefit**: Speed, multiple perspectives, confidence scoring

#### 5. Context Persistence (Background Agents)
```
Task → Agent → Result → Persist (markdown) → Index → Searchable
```

**Key**: Results stored with metadata for future retrieval
**Benefit**: Knowledge accumulation, avoid redundant work

### Parallelism vs. Dependencies

**From Research**: "Interdependent steps require sequential execution. When one action depends on the output of another, forcing parallelism introduces coordination risk and increases likelihood of errors."

**DAG Management**: Modern workflow orchestration systems manage directed acyclic graphs (DAGs) of AI operations for reliable execution at scale.

**Legion Application**:
- **Parallel**: Multiple issues in backlog → spawn multiple workers
- **Sequential**: Issue dependencies → wait for blocking issues to complete
- **Hybrid**: Same issue, multiple review agents in parallel

## 11. Learnings for Legion Architecture

### What Legion Already Does Well

1. **Persistent Controller Daemon** - Similar to OpenCode's server mode
2. **Isolated Workspaces** (jj) - Similar to OpenCode's Docker workspace support
3. **Issue State Machine** - Clear lifecycle like OpenCode's agent loop
4. **Specialized Modes** (architect, plan, implement, review, retro) - Similar to agent specialization

### What Legion Could Adopt

#### From Oh My OpenCode
- **Three-tier agent hierarchy** with explicit capability boundaries
- **Two-mode agents** (primary vs subagent) to prevent recursive delegation
- **Parallel execution pattern** - put independent subagent calls in single message

#### From OpenAgentsControl
- **MVI (Minimal Viable Information)** - Only load what's needed, keep context under 200 lines
- **Token efficiency** via lazy loading
- **Team-ready patterns** - commit agent configurations to repo

#### From Background Agents
- **Context persistence** - Store research/planning results as markdown with metadata
- **Async delegation** - Worker continues while subagent researches
- **Searchable history** - Index past decisions for future reference

#### From Multi-Agent Review
- **Cross-model review** - Use different model families for diverse perspectives
- **Parallel reviewer deployment** - Speed + depth
- **Confidence scoring** - Aggregate opinions for reliability

#### From Aider
- **Architect/Editor pattern** for backlog phase:
  - Architect model: Generate spec-ready issue breakdown
  - Editor model: Validate spec format and completeness

### OpenCode Integration Roadmap

#### Phase 1: Proof of Concept
1. Create `legion review-cross-model` skill
2. Invoke OpenCode CLI with single model (e.g., GPT-4o)
3. Parse JSON output, post to PR comment
4. Validate feasibility

#### Phase 2: Multi-Model Review
1. Configure OpenCode with 3 model variants (security, performance, general)
2. Implement parallel execution pattern
3. Build synthesis/deduplication logic
4. Generate confidence scores

#### Phase 3: Production Integration
1. Integrate into Legion worker review phase
2. Add Linear issue commenting with review summary
3. Add PR labels based on findings
4. Implement on-demand triggers (`/review-cross-model`)

#### Phase 4: Advanced Features
1. Context persistence for review history
2. Model-specific scoring (track which models catch which issues)
3. Adaptive reviewer selection based on PR content
4. Cost optimization (only invoke needed reviewers)

## 12. Technical Feasibility Assessment

### OpenCode as Review Bot: HIGH FEASIBILITY

**Pros**:
- ✅ CLI non-interactive mode with JSON output
- ✅ Server API for programmatic control
- ✅ Multi-model support (75+ providers)
- ✅ Mature community orchestration patterns
- ✅ MCP integration (can use Linear directly)
- ✅ No vendor lock-in

**Cons**:
- ⚠️ Requires separate OpenCode installation
- ⚠️ Additional API costs for non-Anthropic models
- ⚠️ Configuration complexity (multiple models)
- ⚠️ Synthesis logic must be implemented in Legion

**Effort Estimate**:
- POC: 4-8 hours (single model, basic integration)
- Multi-model: 2-3 days (parallel execution, synthesis)
- Production: 1 week (error handling, monitoring, docs)

### Alternative: Continue.dev

**Pros**:
- ✅ CLI available for programmatic invocation
- ✅ Multi-model support
- ✅ MCP integration
- ✅ Pre-configured workflow templates

**Cons**:
- ⚠️ Primarily IDE-focused (VS Code/JetBrains)
- ⚠️ CLI mode less mature than OpenCode
- ⚠️ Smaller community for orchestration patterns

**Verdict**: OpenCode is better fit for Legion's terminal-first architecture.

### Alternative: Aider

**Pros**:
- ✅ Terminal-native
- ✅ Git-first (works well with jj)
- ✅ Multi-model support

**Cons**:
- ⚠️ Designed for pair programming, not review
- ⚠️ No clear multi-agent orchestration patterns
- ⚠️ Would require more custom orchestration

**Verdict**: Good for implementation, less suited for review orchestration.

## 13. References

### Documentation
- [OpenCode CLI](https://opencode.ai/docs/cli/)
- [OpenCode SDK](https://opencode.ai/docs/sdk/)
- [OpenCode Server](https://opencode.ai/docs/server/)
- [OpenCode MCP Servers](https://opencode.ai/docs/mcp-servers/)
- [OpenCode Agents](https://opencode.ai/docs/agents/)
- [OpenCode Models](https://opencode.ai/docs/models/)
- [OpenCode Providers](https://opencode.ai/docs/providers/)
- [Aider Chat Modes](https://aider.chat/docs/usage/modes.html)
- [Continue.dev Docs](https://docs.continue.dev)

### GitHub Repositories
- [opencode-ai/opencode](https://github.com/opencode-ai/opencode)
- [code-yeongyu/oh-my-opencode](https://github.com/code-yeongyu/oh-my-opencode)
- [darrenhinde/OpenAgentsControl](https://github.com/darrenhinde/OpenAgentsControl)
- [kdcokenny/opencode-background-agents](https://github.com/kdcokenny/opencode-background-agents)
- [kdcokenny/opencode-workspace](https://github.com/kdcokenny/opencode-workspace)
- [Aider-AI/aider](https://github.com/Aider-AI/aider)
- [continuedev/continue](https://github.com/continuedev/continue)

### Articles and Blog Posts
- [Boosting AI Coding Productivity with Multi-Model Agents: A Deep Dive into Oh My OpenCode](https://thamizhelango.medium.com/boosting-ai-coding-productivity-with-multi-model-agents-a-deep-dive-into-oh-my-opencode-25ebaf0e8d6b)
- [One Reviewer, Three Lenses: Building a Multi-Agent Code Review System with OpenCode](https://blog.devgenius.io/one-reviewer-three-lenses-building-a-multi-agent-code-review-system-with-opencode-21ceb28dde10)
- [Separating code reasoning and editing (Aider)](https://aider.chat/2024/09/26/architect.html)
- [How Coding Agents Actually Work: Inside OpenCode](https://cefboud.com/posts/coding-agents-internals-opencode-deepdive/)
- [Building Cloud Agents with Continue CLI](https://blog.continue.dev/building-async-agents-with-continue-cli/)

### Comparisons
- [Claude Code vs OpenCode: Which Agentic CLI Fits Your Workflow?](https://www.infralovers.com/blog/2026-01-29-claude-code-vs-opencode/)
- [OpenCode vs Claude Code vs OpenAI Codex: A Comprehensive Comparison](https://bytebridge.medium.com/opencode-vs-claude-code-vs-openai-codex-a-comprehensive-comparison-of-ai-coding-assistants-bd5078437c01)
- [OpenCode vs Claude Code: 2026 Battle Guide](https://byteiota.com/opencode-vs-claude-code-2026-battle-guide-48k-vs-47k/)
- [OpenCode vs Claude Code vs Cursor: I Tested All 3 for 30 Days](https://www.nxcode.io/resources/news/opencode-vs-claude-code-vs-cursor-2026)

### Architecture Patterns
- [AI Agent Orchestration Patterns - Azure Architecture Center](https://learn.microsoft.com/en-us/azure/architecture/ai-ml/guide/ai-agent-design-patterns)
- [Multi-Agent AI Systems: Orchestrating AI Workflows](https://www.v7labs.com/blog/multi-agent-ai)
- [Parallel Agent Processing](https://www.kore.ai/blog/parallel-ai-agent-processing)
- [Building Effective AI Agents (Anthropic)](https://www.anthropic.com/research/building-effective-agents)

### Community Resources
- [Awesome Opencode](https://awesome-opencode.com/)
- [Agent Orchestration Overview - DeepWiki](https://deepwiki.com/code-yeongyu/oh-my-opencode/4.1-agent-orchestration-overview)

## 14. Conclusion

OpenCode and its ecosystem provide mature, battle-tested patterns for multi-agent orchestration that align well with Legion's architecture. The key insights:

1. **Cross-model review is highly feasible** via OpenCode's CLI/SDK/Server interfaces
2. **Parallel execution patterns** are well-understood (put independent calls in single message)
3. **Agent hierarchy** (primary → advisory → execution) prevents recursive delegation issues
4. **Context persistence** with metadata enables knowledge accumulation
5. **Synthesis and confidence scoring** from multiple model perspectives improves reliability

Legion should start with a proof-of-concept OpenCode integration for cross-model-family review, then expand to full multi-agent orchestration patterns based on Oh My OpenCode's architecture.

The separation between Claude Code (speed, polish, opinionated) and OpenCode (flexibility, multi-model, explicit control) suggests they're complementary rather than competing - Legion can use Claude Code for implementation and OpenCode for cross-model validation.
