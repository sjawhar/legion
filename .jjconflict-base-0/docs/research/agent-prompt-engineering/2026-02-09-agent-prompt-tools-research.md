# Agent Prompt Engineering Tools & Resources

**Date:** 2026-02-09
**Context:** Research into well-regarded tools and resources for writing Claude Code / OpenCode agent prompts, skills, and sub-agent definitions. Focused on resources from Sep 2025 onwards (latest models).
**Goal:** Find existing tools or resources that produce high-quality agent prompts, with strong community validation.

---

## Executive Summary

There is no single "gold standard" tool the community universally agrees produces high-quality agent prompts. The **Skill Factory** is the closest to a generator, but it's more of a scaffolder than a quality optimizer. The best practitioners combine principles from ClaudeLog's Agent Engineering guide with Anthropic's official skill-building docs, browse high-star collections for patterns, use a generator for initial scaffolding, then iterate heavily by hand.

The ecosystem gap is a tool that combines **generation with quality evaluation** — something that produces an agent prompt and then scores/validates it against best practices. That doesn't exist yet.

---

## Tier 1: Generator Tools (Community-Validated)

### 1. Claude Code Skill Factory
- **URL:** https://github.com/alirezarezvani/claude-code-skill-factory
- **Stars:** ~478 | **Forks:** 93 | **License:** MIT
- **Last Updated:** Oct 2025 (v1.4.0)
- **What it does:** Most comprehensive meta-tool for generating agent prompts, skills, slash commands, and hooks. Includes an Agent Factory sub-skill that generates properly-formatted `.md` agent definitions with YAML frontmatter.
- **Features:**
  - 5 interactive guide agents (factory-guide, skills-guide, prompts-guide, agents-guide, hooks-guide)
  - 10 slash commands (`/build skill`, `/build agent`, `/build prompt`, `/build hook`, etc.)
  - 69 prompt presets across 15 domains (via Prompt Factory skill)
  - Multi-format output (XML, Claude, ChatGPT, Gemini)
  - 7-point quality validation system
  - Cross-tool compatibility (Claude Code ↔ Codex CLI bridge)
- **Also available on:** [claude-plugins.dev](https://claude-plugins.dev/skills/@alirezarezvani/claude-code-skill-factory/agent-factory), [skillsdirectory.com](https://skillsdirectory.com/skills/alirezarezvani-agent-factory)
- **Companion repos:**
  - [claude-code-tresor](https://github.com/alirezarezvani/claude-code-tresor) — ready-to-use workflow tools (8 skills, 8 agents, 4 commands)
  - [claude-skills](https://github.com/alirezarezvani/claude-skills) — 37+ domain-specific production skills
- **Author's guide:** https://alirezarezvani.medium.com/claude-skills-tutorials-toolkit-7-steps-how-to-actually-ship-fully-customized-ai-for-your-needs-c0dc47101046
- **Assessment:** Best starting scaffold. Generated output can be generic/over-templated — treat as starting point, not final product.

### 2. Agent Skill Creator
- **URL:** https://github.com/FrancyJGLisboa/agent-skill-creator
- **Stars:** ~231 | **Forks:** 35
- **What it does:** A focused meta-skill that teaches Claude Code to create complete agents autonomously. Lighter-weight than Skill Factory — does one thing (agent generation) well.
- **Assessment:** Good for a simpler, less opinionated approach to agent generation.

### 3. Skill Builder
- **URL:** https://github.com/metaskills/skill-builder
- **Stars:** ~71 | **Forks:** 13
- **What it does:** Minimalist approach to building Claude Code agent skills.
- **Assessment:** Smaller community but clean approach.

### 4. RchGrav's 3-Agent Meta Prompt (Atlas/Mercury/Apollo)
- **URL:** https://gist.github.com/RchGrav/438eafd62d58f3914f8d569769d0ebb3
- **Guide:** https://hypeflo.ws/workflow/agentic-meta-prompt-for-claude-code-3-agent-system-generator
- **What it does:** A ~130-line meta prompt that generates a minimal 3-agent system using Blackboard Architecture. Shared context via `context.md`, quality-driven iteration.
- **Assessment:** Interesting architectural pattern for orchestrated multi-agent systems rather than standalone agents.

---

## Tier 2: Design Principles & Learning Resources

### 5. ClaudeLog — Agent Engineering Guide
- **URL:** https://claudelog.com/mechanics/agent-engineering/
- **Author:** InventorBlack (Wilfred Kasekende), CTO at Command Stick, mod at r/ClaudeAI
- **Last Updated:** Jan 16, 2026
- **Why it matters:** The single best resource on agent prompt design **principles** for Claude Code. Not a generator, but the knowledge you need to evaluate generated prompts.
- **Key topics covered:**
  - Token budgeting per agent (lightweight <3k, medium 10-15k, heavy 25k+)
  - Tool count vs. initialization cost (empirical data table)
  - Model selection strategy: Haiku 4.5 (90% of Sonnet's agentic perf at 2x speed, 3x cost savings)
  - "big.LITTLE" orchestration pattern (Sonnet orchestrator + Haiku workers)
  - Auto-delegation via description SEO ("use PROACTIVELY", "MUST BE USED")
  - Agent chainability and composability
  - Agent nicknaming for efficiency
- **Related pages:** [Custom Agents](https://claudelog.com/mechanics/custom-agents/), [Sub-Agents](https://claudelog.com/mechanics/sub-agents/), [Split Role Sub-Agents](https://claudelog.com/mechanics/split-role-sub-agents/)
- **Assessment:** Read this first. Foundational knowledge for anyone writing agent prompts.

### 6. Anthropic's Official "Complete Guide to Building Skills for Claude"
- **URL:** https://resources.anthropic.com/hubfs/The-Complete-Guide-to-Building-Skill-for-Claude.pdf
- **Format:** PDF (~30 pages)
- **Topics:** Planning and design, testing and iteration, distribution and sharing, patterns and troubleshooting
- **Assessment:** The canonical reference for skill structure. Dense but authoritative.

### 7. Anthropic's Prompt Generator
- **URL:** https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/prompt-generator
- **What it does:** Anthropic's built-in tool for generating first-draft prompt templates via the API/Console.
- **Assessment:** General-purpose (not agent-specific), but useful as a starting point.

### 8. Anthropic's Prompting Best Practices
- **URL:** https://platform.claude.com/docs/en/build-with-claude/prompt-engineering/claude-prompting-best-practices
- **Assessment:** Core prompting principles that apply to all agent prompts.

### 9. Claude's Context Engineering Secrets (Bojie Li)
- **URL:** https://01.me/en/2025/12/context-engineering-from-claude/
- **Date:** Dec 20, 2025
- **What it is:** Notes reconstructed from Anthropic team talks at AWS re:Invent 2025
- **Topics:** Skills, Agent SDK, MCP, evaluation systems, context management
- **Assessment:** Direct Anthropic team insights. Good for understanding the "why" behind skill architecture.

---

## Tier 3: Agent Collections (Pattern Reference)

Use these to see what good agent prompts look like across many authors.

### 10. awesome-claude-agents (rahulvrane)
- **URL:** https://github.com/rahulvrane/awesome-claude-agents
- **Stars:** ~279
- **What it is:** Comprehensive directory of agent collections, frameworks, guides, and video tutorials.
- **Top collections cataloged:**
  - [0xfurai/claude-code-subagents](https://github.com/0xfurai/claude-code-subagents) — 100+ agents, uniform format
  - [wshobson/agents](https://github.com/wshobson/agents) — 48 production-ready agents
  - [vijaythecoder/awesome-claude-agents](https://github.com/vijaythecoder/awesome-claude-agents) — 26 agents, AI dev team
  - [davepoon/claude-code-subagents-collection](https://github.com/davepoon/claude-code-subagents-collection) — 36 agents
  - [charles-adedotun/claude-code-sub-agents](https://github.com/charles-adedotun/claude-code-sub-agents) — full lifecycle

### 11. ccprompts
- **URL:** https://github.com/ursisterbtw/ccprompts
- **Stars:** ~65
- **What it is:** 70+ commands across 12 dev phases, 10 specialized agents, Dagger-based safety system.
- **Assessment:** Practical, opinionated, production-oriented. Good reference for structuring a large agent system.

### 12. claud-skills (Interstellar-code)
- **URL:** https://github.com/Interstellar-code/claud-skills
- **What it is:** Production-ready framework with 13 agents, 9 skills, auto-generated docs for JS/TS/PHP/Laravel/React/Python.

### 13. agent-skills-guide (zebbern)
- **URL:** https://github.com/zebbern/agent-skills-guide
- **Stars:** ~10
- **What it is:** Guide for creating agent skill files with examples, templates, and best practices. Based on official sources + community feedback.
- **Assessment:** Small but clean — good structural reference.

---

## Tier 4: Blog Posts & Deep Dives

### General Guides
- **Sankalp's Guide to Claude Code 2.0** (Dec 2025): https://sankalp.bearblog.dev/my-experience-with-claude-code-20-and-how-to-get-better-at-using-coding-agents/
  - Covers skills, hooks, reminders, sub-agent design, context engineering
- **Addy Osmani's LLM Coding Workflow** (Dec 2025): https://medium.com/@addyosmani/my-llm-coding-workflow-going-into-2026-52fe1681325e
  - 90% of Claude Code written by Claude Code; practical workflow advice
- **PubNub's Sub-agent Best Practices** (Aug 2025): https://www.pubnub.com/blog/best-practices-for-claude-code-sub-agents/
  - Real enterprise production experience migrating to sub-agent pipeline
- **Arize CLAUDE.md Best Practices** (Nov 2025): https://arize.com/blog/claude-md-best-practices-learned-from-optimizing-claude-code-with-prompt-learning/
  - Context engineering layer that feeds agents

### Meta-Agent / Agent Factory Tutorials
- **Dr. Ernesto Lee — "Build a Meta-Agent Factory"** (Jan 2026): https://drlee.io/build-a-meta-agent-factory-use-claude-code-to-create-an-ai-that-builds-other-ais-fcaae34496cf
- **Reza Rezvani — "From Subagents to Agent Teams"** (Feb 2026): https://alirezarezvani.medium.com/from-subagents-to-agent-teams-claude-codes-multi-agent-leap-and-what-i-actually-change-97edf83a4d5e
- **MLearning.ai — "Claude Skills: 50+ Power Tips"** (Oct 2025): https://mlearning.substack.com/p/claude-agent-skills-50-power-tips-tricks-guide-anthropic (paywalled)

### Video
- **"Best Practices for Claude Code: Sub-Agents, API Gateways"** (Dec 2025): https://www.youtube.com/watch?v=VObak8yeowc

---

## Platforms & Marketplaces

- **Claude Code Plugins (community marketplace):** https://claude-plugins.dev/
- **Skills Directory:** https://skillsdirectory.com
- **Agent Factory (on Panaversity):** https://agentfactory.panaversity.org/
- **Anthropic's official skills repo:** https://github.com/anthropics/skills

---

## Key Takeaways for Legion

1. **No single generator produces production-quality output** — all require iteration
2. **Token budget is critical** — ClaudeLog's data shows tool count directly impacts init cost (0 tools = 640 tokens, 15+ tools = 13.9k-25k tokens)
3. **Description field is "Tool SEO"** — use "PROACTIVELY" and "MUST BE USED" for auto-delegation reliability
4. **Skill Factory is the best scaffold** — use it to generate initial structure, then hand-tune
5. **big.LITTLE pattern maps to Legion** — Sonnet/Opus orchestrator + Haiku workers for cost efficiency
6. **The gap we could fill** — a generator that also validates/scores against best practices
