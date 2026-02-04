# Workflow Orchestration Research

**Date:** 2026-02-06
**Context:** Comparative analysis of Legion's workflow against production AI coding orchestration systems
**Sources:** Blitzy podcast transcript + SWE-bench paper, Oh My OpenCode, Claude Code Agent Teams, OpenCode, Reddit/community reports

## Executive Summary

We analyzed Legion's architecture against three production systems (Blitzy, Oh My OpenCode, Claude Code Agent Teams) and community best practices. Legion's core architecture is sound — planning-heavy pipeline, TDD enforcement, isolated workspaces, retro-based learning. The biggest opportunities are: (1) cross-model diversity, (2) more dynamic workflow routing, (3) impact analysis, and (4) E2E testing. Issues filed: LEG-66 through LEG-71, LEG-77.

---

## Systems Analyzed

### Blitzy (Enterprise AI Coding Platform)
- 3,000+ specialized agents running 8-12 hour inference windows
- Deep codebase knowledge graph (days of compute to build)
- Dynamic agent generation (prompts written by agents, tools selected JIT)
- Cross-family model review (always different family for review vs generation)
- Actually builds and runs applications for QA validation
- 80-90% autonomous completion, structured human handoff for remainder
- 86.8% Pass@1 on SWE-bench Verified (13% above prior best)

#### Technical Architecture (from SWE-bench paper)

**Code Ingestion → Relational Index:**
- Entire source corpus transformed into a "hierarchically summarized, relational index"
- Each line of code semantically embedded while preserving full relational dependencies:
  control-flow, call graphs, inheritance hierarchies, module dependencies
- Language-agnostic intermediate schema — cross-language dependencies preserved
  (e.g., Python→C extensions, COBOL→Java service interactions)
- Designed for AI agent traversal, not human consumption

**JIT Context Management:**
- Ranking-based retrieval using both semantic similarity AND relational proximity
- "Effectively extends the usable context without bound" — only most contextually useful
  fragments surfaced per query
- Not RAG alone, not knowledge graph alone — hybrid ranking system

**Per-File Validation Loop:**
- Every file edit triggers: generate ad-hoc tests → run tests → write change → recompile → rerun tests
- Deviation from "pass-to-pass" on any test triggers dynamic validation workflow
- At end of all changes: recompile + run ALL existing tests in source code

**Multi-Model Architecture:**
- "Proprietary multi-agent, multi-model architecture leveraging combination of best-in-class
  generative AI models (Anthropic, OpenAI, Google)"
- "Most suitable model for the relevant purpose (e.g., one LLM for code generation,
  another for pre-compilation or validation)"

**Benchmark Workflow (7 steps):**
1. Fork repos, create branch per issue at original commit
2. Create Blitzy project per issue
3. Provide branch URL → ingestion agents create internal mapping + technical specification
4. Prompt with problem statement only → agents update tech spec with understanding + potential fixes
5. Code generation agents complete changes, create PR + project guide documenting implementation
6. Single-shot (Pass@1), no best-of-k, no scaffolding, no prompt enhancement

**Key design principle:** "Blitzy's architecture is intended to optimize for code quality, not costs."

### Oh My OpenCode (Open-Source Agent Harness)
- Three-tier hierarchy: Primary (Orchestrator) → Advisory (Strategist) → Execution (Specialist)
- Planning triad: Prometheus (plan) + Metis (gap analysis) + Momus (validation)
- 11+ specialized agents using different models per role
- Categories over model names for task delegation (avoids bias)
- Background parallel agents for exploration while main agent strategizes
- 25+ hooks for lifecycle management

### Claude Code Agent Teams (Anthropic, Feb 5 2026)
- Built-in task lists with dependency tracking and auto-unblocking
- Native inter-agent messaging (DMs + broadcasts)
- Plan approval mode (worker submits plan, lead approves/rejects)
- Delegate mode (forces lead to coordinate, not implement)
- File-lock-based task claiming
- C compiler case study: 16 agents, 2K sessions, $20K

---

## Where Legion Aligns with Best Practices

### 1. Planning-Heavy Pipeline
All systems agree: planning should consume the majority of agent time.
- **Blitzy:** "We spend a huge amount of time in planning... code generation is relatively fast"
- **Oh My OpenCode:** Prometheus interviews, Metis gap-analyzes, Momus validates — 3 agents before any code
- **Legion:** 7-step plan workflow with research, deepening, review (3 iterations), executable task generation

### 2. Workspace Isolation
Every system uses isolated workspaces for parallel agents.
- **Blitzy:** Parallel cloud environments per enterprise app
- **Oh My OpenCode:** Background tasks with independent context
- **Claude Code Teams:** Each teammate has isolated context window
- **Community:** Git worktrees are "the secret weapon" for parallel AI agents
- **Legion:** jj workspaces per issue — same pattern, arguably best VCS for it

### 3. TDD as Structural Guarantee
Community consensus: "Elaborate system prompts telling AI to write tests first don't work — need systematic enforcement via tools."
- **Legion:** /superpowers:test-driven-development as mandatory step in implement workflow
- **Oh My OpenCode:** Language server diagnostics must pass before task completion
- **Blitzy:** Unit tests before/after every file touch, integration tests between services

### 4. Separate Review from Implementation
All systems separate generation from evaluation.
- **Blitzy:** Different model family required for review
- **Oh My OpenCode:** Atlas "NEVER trusts subagent claims" — verifies independently
- **Claude Code Teams:** Separate review agents dispatched
- **Legion:** Separate reviewer worker in fresh session with multi-agent parallel review

### 5. Institutional Memory
- **Blitzy:** "Much more bullish on memory than fine-tuning" — stored at application layer
- **Oh My OpenCode:** Atlas accumulates wisdom from task to task, forwards learnings
- **Legion:** Dual-perspective retro (context-free + full-context), docs/solutions/ with oracle retrieval

### 6. Escalation Before Blocking
- **Blitzy:** Independent evaluation system generates human completion reports
- **Oh My OpenCode:** Forced completion hooks prevent agents from quitting mid-task
- **Legion:** Oracle sub-skill researches before escalating to user-input-needed

---

## Key Gaps and Opportunities

### Gap 1: Cross-Model Diversity (LEG-68)

**What everyone else does:**
- Blitzy: Different model family required for review
- Oh My OpenCode: 11 agents across Claude, GPT, Gemini, Grok, GLM
- Community: "Sonnet for volume, Opus for complexity" — even within-family diversity helps

**What Legion does:** Claude for everything.

**Best approach for Legion:**
- OpenCode CLI is the most viable integration path: `opencode -p "prompt" --model google/gemini-3-pro -f json`
- Oh My OpenCode's "categories over model names" pattern avoids bias — delegate by purpose, not brand
- Minimum viable: Add one non-Claude review pass. Maximum: Oh My OpenCode-style multi-model routing.

### Gap 2: Dynamic Workflow Routing (LEG-77)

**What everyone else does:**
- Blitzy: Dynamic agent generation, prompts written by agents, tool selection JIT
- Oh My OpenCode: Categories + skills system routes tasks to appropriate agents
- Claude Code Teams: Plan approval mode, delegate mode, self-claiming tasks

**What Legion does:** Fixed state machine: architect → plan → implement → review → retro → merge, always in order.

**Specific patterns worth adopting:**

1. **Controller intelligence at triage** — Controller should assess issue complexity and route:
   - Simple/clear → skip architect, maybe skip planning
   - Complex/vague → full pipeline
   - Too big → architect breaks down before anything else

2. **Worker self-assessment** — Implementer discovers issue is bigger than planned:
   - Should be able to signal "needs re-planning" (not just user-input-needed)
   - Could dispatch sub-agents for parallel sub-tasks (Claude Code team feature)

3. **Oh My OpenCode's category system** — Instead of fixed modes, tag issues with semantic categories:
   - `quick`: Trivial single-file fix → skip planning
   - `refactor`: Needs impact analysis → full pipeline
   - `frontend`: Needs different skills/models
   - Categories determined by architect or controller

4. **Incremental adoption path:**
   - Phase 1: Add "skip conditions" to each workflow (explicit in SKILL.md)
   - Phase 2: Controller assesses complexity, chooses workflow template
   - Phase 3: Workers can request re-routing via new label

### Gap 3: Impact Analysis (LEG-67)

**What Blitzy does:** Deep impact analysis before implementation — what will this change affect across the system?

**What Oh My OpenCode does:** Metis agent performs pre-planning gap analysis — identifies edge cases, implicit assumptions, missing context.

**What Legion should do:** Add impact analysis step to plan workflow:
1. Identify files the plan will touch
2. Map imports/callers of affected modules
3. Check if plan accounts for updating dependents
4. Use `jj log --file` for recent change hotspots
5. Feed findings into plan review

### Gap 4: E2E Testing (LEG-69)

**What Blitzy does:** Actually builds and runs enterprise apps, QA agents click through and screenshot.

**What Oh My OpenCode does:** Language server diagnostics must pass; forced completion hooks ensure agents don't skip validation.

**What Legion should do:** Dedicated tester worker mode. E2E test scenarios defined by architect, test steps planned by planner, executed by tester after implementation.

### Gap 5: Structured Escalation (LEG-70)

**What Blitzy does:** "Human completion report" — tasks remaining, estimated hours, recommended skillset.

**What Oh My OpenCode does:** Atlas tracks task status and accumulates context about what failed and why.

**What Legion should do:** Structured escalation template (phase, completed work, blocker, options considered, remaining estimate, expertise needed).

### Gap 6: Parallelism Analysis (LEG-71)

**What Blitzy does:** Algorithmically determines sequential vs parallel at planning time. Defaults to sequential when uncertain.

**What Oh My OpenCode does:** Atlas identifies parallelizable task groups. Single-message parallel subagent calls vs sequential messages.

**What Legion should do:** Planner annotates task dependencies and parallelizability. Implementer can override with its own analysis.

### Gap 7: Human Approval on Requirements (LEG-66)

**What Blitzy does:** Human approves spec before any implementation.

**What Oh My OpenCode does:** User reviews plan files, explicitly triggers /start-work.

**What Legion does:** Controller auto-transitions after architect.

**What Legion should do:** Pause after architect adds worker-done. Wait for human approval before dispatching planner. This is for requirements only, not implementation plans.

---

## Patterns Worth Adopting

### Oh My OpenCode's Planning Triad
- **Prometheus** (create plan) + **Metis** (gap analysis) + **Momus** (validate plan)
- Legion equivalent: /workflows:plan (create) + impact analysis agent (gap) + /plan_review (validate)
- The gap analysis step is what we're missing — Metis checks for "implicit assumptions, edge cases, missing context"

### Oh My OpenCode's Wisdom Accumulation
- Atlas forwards learnings from completed tasks to subsequent tasks
- Legion equivalent: retro captures learnings to docs/solutions/, oracle surfaces them
- Enhancement: Controller could inject recent learnings from same codebase area into new worker context

### Claude Code Teams' Plan Approval Mode
- Worker works in read-only plan mode until lead approves
- Maps directly to LEG-66: architect produces requirements, controller pauses for human approval
- Native API support means less custom code than Linear label-based approach

### Oh My OpenCode's Forced Completion
- Hooks prevent agents from quitting mid-task
- System reminders: "You have incomplete todos! Complete ALL before responding."
- Legion's stuck detection kills workers after 10 min inactivity — opposite approach
- Consider: add persistence reminders before killing (resume with "you left todos incomplete")

### Community's Cross-Model Tiering
- Haiku for setup/exploration, Sonnet for implementation, Opus for review/debugging
- Real report: Sonnet suggested circular fixes on 47-file migration; Opus solved it in 2 prompts touching 4 files
- Within-family diversity already helps; cross-family (via OpenCode) adds another dimension

---

## Research Gaps (Future Investigation)

1. **Blitzy's knowledge graph implementation details** — The SWE-bench paper confirms the architecture (hierarchically summarized relational index, language-agnostic intermediate schema, JIT ranking-based retrieval) but doesn't reveal the specific data structures, query algorithms, or how the index is maintained as code changes. Their domain-specific context engineering paper referenced in the podcast is still not publicly accessible.

2. **Claude Code Agent Teams in practice** — Released Feb 5, 2026. Too new for community experience reports beyond Anthropic's own C compiler case study.

3. **Cost modeling** — Anthropic research shows multi-agent systems use 15x more tokens for 90% better results. Need to model Legion's token costs per issue type to optimize.

4. **Oh My OpenCode integration details** — Their orchestration system is well-documented but we'd need hands-on testing to evaluate practical integration with Legion's jj-workspace-based architecture.

---

## Action Items (Linear Issues)

| Issue | Title | Priority |
|-------|-------|----------|
| LEG-66 | Human approval gate on architect outputs | High |
| LEG-67 | Impact analysis step in plan workflow | Medium |
| LEG-68 | Cross-family model review via OpenCode | Medium |
| LEG-69 | Dedicated tester worker with E2E validation | Medium |
| LEG-70 | Structured escalation reports sub-skill | Medium |
| LEG-71 | Upfront parallelism annotation in plans | Medium |
| LEG-77 | Dynamic workflow orchestration exploration | Medium |

---

## Key Sources

### Primary
- [Blitzy Cognitive Revolution Podcast Transcript](https://www.cognitiverevolution.ai/infinite-code-context-ai-coding-at-enterprise-scale-w-blitzy-ceo-brian-elliott-cto-sid-pardeshi/)
- [Oh My OpenCode GitHub](https://github.com/code-yeongyu/oh-my-opencode)
- [Oh My OpenCode Orchestration Guide](https://github.com/code-yeongyu/oh-my-opencode/blob/dev/docs/guide/understanding-orchestration-system.md)
- [Claude Code Agent Teams Docs](https://code.claude.com/docs/en/agent-teams)
- [Building a C Compiler with Parallel Claudes](https://www.anthropic.com/engineering/building-c-compiler)

### Secondary
- [Claude Code Hooks Guide](https://code.claude.com/docs/en/hooks-guide)
- [OpenCode vs Claude Code](https://www.builder.io/blog/opencode-vs-claude-code)
- [Parallel AI Agent Coding (Pragmatic Engineer)](https://blog.pragmaticengineer.com/new-trend-programming-by-kicking-off-parallel-ai-agents/)
- [Spec-Driven Development (Martin Fowler)](https://martinfowler.com/articles/exploring-gen-ai/sdd-3-tools.html)
- [METR Developer Productivity Study](https://metr.org/blog/2025-07-10-early-2025-ai-experienced-os-dev-study/)
- [Addy Osmani's LLM Coding Workflow](https://addyosmani.com/blog/ai-coding-workflow/)
- [Multi-Model Deep Dive (Oh My OpenCode)](https://thamizhelango.medium.com/boosting-ai-coding-productivity-with-multi-model-agents-a-deep-dive-into-oh-my-opencode-25ebaf0e8d6b)
