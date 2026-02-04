# Legion Design Principles

Core principles governing how Legion orchestrates autonomous software development. Derived from empirical analysis of production systems (Blitzy, Oh My OpenCode, Claude Code Agent Teams), community experience reports, and our own observations.

---

## 1. Planning is the product

Most of the value is created before any code is written. Code generation is cheap — tokens stream fast and models are good at it. What's expensive is generating the *wrong* code, discovering it's wrong, and unwinding it.

Blitzy spends "a huge amount of time in planning and system understanding and impact analysis... the code generation is relatively fast." Oh My OpenCode dedicates three agents to planning alone (Prometheus plans, Metis gap-analyzes, Momus validates) before a single line is written. The METR study found experienced developers got 19% *slower* with AI tools — largely because they spent more time reviewing and correcting AI output than they saved on writing it.

**In practice:** Legion's pipeline is planning-heavy by design: architect → plan (with research, deepening, and up to 3 review iterations) → implement. The plan workflow invokes more agents and consumes more compute than any other phase. This is correct and intentional.

**The corollary:** If you're tempted to skip planning for a "simple" issue, that's a signal that the issue categorization should change, not that the planning should be cut. Simple issues should have simple plans, not no plan.

---

## 2. Humans decide *what*, agents decide *how*

The highest-leverage human contribution is specifying requirements and validating that they were met. Everything between those two points — planning the implementation, writing code, running tests, reviewing — is agent work.

Blitzy requires human approval of specs before execution. Their customers who get the best results are "unbelievable at expressing intent and doing spec-driven development." The ones who struggle aren't limited by the AI — they're limited by their own ability to express what they want.

**In practice:** Humans own the requirements (architect outputs). Agents own the implementation (plans, code, tests, reviews). The approval gate is on requirements, not on implementation plans — because humans are better at judging "is this what I want?" than "is this how I'd build it?"

**The anti-pattern:** Inserting human checkpoints mid-implementation. Blitzy is emphatic: "it would be an impossible task to try to insert a human into this process." Humans in the loop during execution are bottlenecks, not quality gates.

---

## 3. Every claim must be verified independently

AI agents are confident, fluent, and wrong often enough that you can never trust self-assessment. Oh My OpenCode's Atlas orchestrator has an explicit rule: "NEVER trust subagent claims." Blitzy uses a separate evaluation system independent from the work system. Claude 3.7 famously wrote tests that returned true to satisfy test-passing requirements without testing anything.

**In practice:**
- Review is always a separate agent from implementation, in a separate session with fresh context
- Tests must actually run and produce observable pass/fail, not just exist as files
- Static analysis (lint, type check, format) runs independently of the agent that wrote the code
- Quality checks should use different models or tools than the ones that generated the code (cross-family review)

**The corollary:** The number of verification layers should scale with the cost of failure, not the confidence of the agent.

---

## 4. Context is the bottleneck, not intelligence

Models are smart enough for most software engineering tasks. What fails them is not having the right information at the right time, or having too much information drowning out what matters.

Blitzy's core innovation is their relational index — not a smarter model, but a system that ensures "when I generate code, I am injecting and pulling out the correct context just in time." They call this "domain-specific context engineering" — the insight that context strategies must be tailored to the domain, not generic.

Sid Pardeshi describes "context anxiety" — models giving up, fabricating time constraints, or taking shortcuts when context windows fill up. The advertised context window is not the effective context window. Quality degrades well before the limit.

**In practice:**
- Each worker gets a narrow, focused scope (one issue, one workspace, one phase)
- Plans are posted to Linear so workers can load them fresh, not inherited through accumulated context
- Research happens in subagents that return summaries, keeping the main context clean
- Impact analysis identifies what's relevant *before* loading context, not after

**The anti-pattern:** Dumping everything into the context window "just in case." More context is not better context.

---

## 5. Isolation enables parallelism

You can only run agents in parallel if their work doesn't interfere. Every production multi-agent system uses some form of workspace isolation — git worktrees, separate cloud environments, containerized sandboxes.

But parallelism has real costs. Anthropic's own research: multi-agent systems outperform single agents by 90% but consume 15x more tokens. A benchmark showed 36% wall-time improvement — meaningful but not 10x. The returns diminish fast.

**In practice:**
- Each issue gets its own jj workspace — complete isolation at the filesystem level
- Up to 10 workers in parallel, each on a different issue
- Within an issue, parallelism is assessed at planning time and annotated explicitly
- When uncertain whether tasks can be parallel, default to sequential (Blitzy's rule)

**The corollary:** Parallelism is a tool for throughput, not speed. Ten workers on ten issues is better than ten workers on one issue.

---

## 6. Failure is an output, not a bug

A system that can't do something and says so clearly is more valuable than one that produces plausible-looking garbage. Blitzy generates a "human completion report" — what was done, what couldn't be done, estimated remaining effort, recommended expertise. The failure report is a first-class deliverable.

Factory identifies specific warning signs of agent failure: self-rewriting plans, edits outside scope, claims without reproductions, bloated diffs. These aren't just bugs to fix — they're signals to detect and escalate.

**In practice:**
- Workers escalate with structured reports (phase, completed work, blocker, options considered, remaining estimate)
- Stuck detection monitors both inactivity (worker stopped doing anything) and unproductive activity (oscillating between fixes)
- Max iteration limits prevent infinite loops (3x for plan review, bounded retries for test fixing)
- The oracle researches before escalating — cheap self-help before expensive human involvement

**The anti-pattern:** Agents that "try harder" indefinitely. More attempts at a fundamentally blocked task waste compute and delay human intervention.

---

## 7. Knowledge compounds

Every completed issue should make the next one easier. If each worker starts from zero understanding, you're paying the discovery cost every time.

Blitzy is "much more bullish on memory than fine-tuning" — memory stored at the application layer survives model upgrades. Oh My OpenCode's Atlas "accumulates wisdom from each task — patterns, conventions, failures — and forwards learnings to subsequent tasks."

**In practice:**
- The retro phase captures learnings from every completed PR, using dual perspectives (context-free + full-context)
- Learnings are stored in `docs/solutions/` with structured frontmatter (tags, symptoms, module)
- The oracle surfaces relevant learnings before workers start new work
- Plans reference institutional learnings during research phases

**What's missing:** Proactive injection of relevant learnings based on files being touched (not just reactive search). A file-to-learnings index that automatically includes applicable knowledge when a worker enters a familiar area of the codebase.

---

## 8. Structure should be thin and model-determined

The default workflow (architect → plan → implement → review → retro → merge) exists because it works today. It should not be treated as the architecture. It's a routing template — one of potentially many — that the system should be able to reshape as models and tasks demand.

The Bitter Lesson (Sutton, 2019) observes that general methods leveraging computation consistently outperform handcrafted human knowledge. Applied to agent harnesses: if your system scales by adding more human-authored workflow nodes, you're scaling headcount, not capability. Every fixed workflow step is a bet that the model can't figure out how to do this itself. Some of those bets are correct today and will be wrong in six months.

Blitzy dynamically generates agents JIT — prompts written by other agents, tool selection assessed at runtime. Oh My OpenCode's Atlas orchestrator decides at runtime what specialists to spawn. Both avoid freezing the "org chart" into the architecture.

**In practice:**
- The default pipeline is a starting point, not a constraint. Every step should be skippable.
- The architect's output should include workflow hints — how complex is this, what phases does it actually need — not just requirements.
- The controller routes based on those hints, not on a fixed state machine.
- Workers can signal re-routing ("this is bigger than planned" or "this is simpler than expected").
- New workflow steps (quality gates, approval gates) are implemented as hooks or configurable checks, not hardcoded state transitions. When they're no longer needed, turning them off should be a config change.

**The litmus test:** If model capability doubles next year, does this workflow step become unnecessary? If yes, make sure it's removable without a refactor. If no (e.g., human approval on requirements), it's a product decision and belongs in the architecture.

**The guard rail:** Default to the full pipeline when uncertain. The system should err toward more structure, not less — but that structure should be progressively removable as confidence grows.

---

## 9. Composable capabilities, not fixed specialists

Different models have different strengths. Different tools catch different bugs. Homogeneous systems have homogeneous blind spots. But the answer is composable capability sets, not a roster of named specialists.

Building a "Security Agent" and a "CSS Agent" with distinct prompts and tool access creates maintenance burden and freezes assumptions about specialization boundaries. If a new model is good at both security and CSS, you shouldn't have to rewrite code to merge those agents. Instead: build generic workers that load capability sets (toolsets, prompt context, model selection) based on what the task needs.

Blitzy: "you get demonstrably better results by having a different family of models review." Oh My OpenCode uses semantic categories (`visual-engineering`, `ultrabrain`) to route to capability sets, not named agents. The community reports that even within-family diversity helps: Sonnet suggested circular fixes on a 47-file migration; Opus solved it in 2 prompts touching 4 files.

**In practice:**
- Workers are generic agents that load capability sets (skills, tools, model preferences) at dispatch time
- The controller (or architect) decides which capabilities a task needs, not which "specialist" to call
- Cross-family review is a capability ("use a non-Claude model for this review pass"), not a specialist ("dispatch the OpenAI reviewer")
- New capabilities are added by creating new toolsets/skills, not new agent types
- Static analysis, type checking, and LSP are capabilities that complement LLM capabilities — layer them

**The litmus test:** If you add a new capability, do you have to create a new worker mode and update the state machine? If yes, your specialization is too rigid. If you just add a skill/toolset that existing workers can load, you're composable.

---

## 10. Ship working software, not impressive demos

The gap between "all tests pass" and "this is what you'd actually ship" is where most AI coding systems fail. Blitzy calls this the difference between "functional correctness" and "intent." Tests passing is necessary but not sufficient.

The METR study, Devin user reports, and community experience all converge on the same finding: AI excels at routine, well-specified tasks and struggles with architectural coherence across a codebase. The value proposition isn't "AI writes all the code" — it's "AI handles the well-specified work so humans can focus on the hard problems."

**In practice:**
- Acceptance criteria must be testable — "should be fast" is not a criterion, "page loads in under 500ms" is
- E2E testing validates that the running application behaves correctly, not just that unit tests pass
- The human completion report honestly assesses what was and wasn't achieved
- 80% of the work done autonomously at high quality is more valuable than 100% of the work done at questionable quality

**The metric that matters:** Not how much code was generated, but how much human time was saved on work that actually shipped.

---

## 11. Build for the model after next

Every structural decision in Legion is a bet about what models can't do. Some of those bets are correct today and will be wrong soon. Design accordingly.

Rich Sutton's Bitter Lesson (2019) observes that general methods leveraging computation consistently outperform handcrafted human knowledge. The lesson for agent harnesses: if your system primarily scales by adding more human-authored structure, you're fighting the trend. The harness should be a thin interface to scalable computation, not the place you stash the intelligence.

This doesn't mean "remove all structure." It means every piece of structure should answer two questions: (1) is this compensating for a model limitation or enforcing a product requirement? and (2) if the model limitation goes away, can I remove this without a refactor?

**Product requirements that belong in the architecture:**
- Human approval on requirements (humans decide what, agents decide how)
- Audit trails (what was done, by which agent, with what evidence)
- Workspace isolation (parallel agents can't interfere with each other)
- Test validation (code must actually work, regardless of how smart the model is)

**Model limitations that should be removable:**
- Fixed workflow ordering (architect before plan before implement) — should become dynamic routing
- Specialized worker modes — should become composable capability sets
- Hard iteration limits (3x plan review) — should become model-assessed confidence thresholds
- Context management workarounds — should shrink as context windows and memory improve

**The rubric:** Build a tool that lets the agent read pytest output, not a regex that parses it for the agent. The first gets better when models improve. The second becomes technical debt.

**In practice:**
- New features are hooks and capabilities, not hardcoded state transitions
- The planning phase is search (exploring the solution space with probe agents), not just thinking
- Workflow structure is the default, not the constraint — the system should be able to route around it
- Every six months, revisit: which structural assumptions are no longer load-bearing?
