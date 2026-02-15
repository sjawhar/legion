# Streamline Skills: Unified Workflow Skill Set

**Goal:** Replace the overlapping Superpowers + Compound Engineering + sjawhar skill collections with a single cohesive set of 16 skills that map cleanly to Legion's worker phases and are generically useful for any software engineering project.

**Architecture:** Discipline core (6 SP skills) kept as-is. 8 new workflow skills built from the best ideas across all three collections. 2 CE domain skills kept for optional use. Cross-family review baked into phase workflows via agent-to-model-family mappings, not a standalone skill. Skills live in `.claude/skills/` for distribution via plugin marketplace.

**Scope:** Skill authoring only. Does not include changes to worker workflow files (architect.md, plan.md, implement.md, review.md, merge.md) — those will be updated in a follow-up to wire in the new skills.

---

## Context

### What Exists Today

Three overlapping collections totaling ~54 skills/commands:

- **Superpowers (14 skills):** Discipline enforcement — iron laws, red flags, rationalization prevention. Excellent at making agents THINK correctly but no pipeline structure.
- **Compound Engineering (28 commands + 12 skills):** Structured brainstorm→plan→work→review→compound pipeline. Recently updated — `/plan-review` removed, replaced by document-review. Heavy with Rails/iOS/domain-specific content irrelevant to this project.
- **sjawhar (27 skills):** Custom jj-native automation. Battle-tested shipping pipeline (safe-ship→push-pr→watch-ci-merge). No discipline enforcement.

### What's Wrong

1. **Overlap** — brainstorming exists in both SP and CE. Plan writing in both. Git worktrees in both (and we use jj).
2. **Domain cruft** — 6 CE skills are Ruby/Rails-specific. 5 CE commands are iOS/docs/changelog. None relevant here.
3. **Wrong abstractions** — SP has separate skills for subagent-driven-dev, dispatching-parallel-agents, and executing-plans. These should be one skill with progressive disclosure.
4. **Missing integration** — No skill combines swarm dispatch with plan execution. Workers need to manage sub-agent teams but this capability is split across 3+ skills.
5. **Dead references** — Worker workflows reference `/compound-engineering:workflows:review` and `/compound-engineering:plan_review` which CE restructured.

### Design Principles

- **Generically useful** — every skill should work for any software project, not just Legion
- **Progressive disclosure** — start simple, reveal complexity only when needed
- **VCS-agnostic** — no hardcoded git or jj commands in workflow skills (VCS skills are separate)
- **Swarm-capable** — workers can spawn sub-agent swarms; swarm agents cannot spawn sub-swarms (one level deep)
- **Cross-family review is a pattern, not a skill** — each phase bakes it in with agent-type → model-family mappings

---

## Final Skill Inventory (16 skills)

### Discipline Core (keep SP as-is)

| # | Skill                       | Source | Action  |
|---|:----------------------------|:-------|:--------|
| 1 | using-superpowers           | SP     | Keep    |
| 2 | test-driven-development     | SP     | Keep    |
| 3 | systematic-debugging        | SP     | Keep    |
| 4 | verification-before-completion | SP  | Keep    |
| 5 | receiving-code-review       | SP     | Keep    |
| 6 | writing-skills              | SP     | Keep    |

### Workflow Skills (build new)

| # | Skill              | Sources                    | Effort | Used By Phase                     |
|---|:-------------------|:---------------------------|:-------|:----------------------------------|
| 7 | brainstorming      | SP + CE brainstorm cmd     | Med    | Architect                         |
| 8 | writing-plans      | SP + CE plan cmd           | Med    | Planner                           |
| 9 | deepening-plans    | CE deepen-plan             | Low    | Planner (optional)                |
| 10 | executing-work    | CE work + SP TDD + swarms  | High   | Implementer                       |
| 11 | document-review   | CE document-review concept | Med    | Architect, Planner, Implementer   |
| 12 | knowledge-capture | CE compound-docs           | Med    | Retro (write); Arch/Plan (read)   |
| 13 | finishing-work    | SP finishing-a-dev-branch  | Low    | Implementer (ship phase)          |
| 14 | analyze           | sjawhar analyze            | Low    | Implementer, Reviewer             |

### Domain Skills (keep CE as-is)

| #  | Skill                      | Source | Action |
|----|:---------------------------|:-------|:-------|
| 15 | agent-native-architecture  | CE     | Keep   |
| 16 | frontend-design            | CE     | Keep   |

### Automation (sjawhar, stay where they are, wired into workflows)

Not part of this plan's deliverables — these already exist and work:

`using-jj`, `push-pr`, `watch-ci-merge`, `sync-main`, `resolve-conflicts`, `centaur-review`, `test-and-fix`, `load-issue`

---

## Skill-to-Phase Mapping

```
              Architect        Planner          Implementer      Reviewer        Merger          Retro
              ─────────        ───────          ───────────      ────────        ──────          ─────
Discipline    using-superpowers (all phases)
              ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─ ─

Workflow      brainstorming    writing-plans    executing-work   analyze         (inline)        knowledge-
              document-review  deepening-plans  TDD                                              capture
              knowledge-       document-review  debugging
              capture (read)   knowledge-       verification
                               capture (read)   receiving-review
                                                analyze
                                                finishing-work
                                                document-review

Automation                                      push-pr                         watch-ci-merge
                                                                                resolve-conflicts
                                                                                sync-main
```

---

## Tasks

### Task 1: brainstorming

**Create:** `.claude/skills/brainstorming/SKILL.md`

**Takes from SP brainstorming:**
- Hard gate: MUST brainstorm before any creative/feature work
- Red flag table (rationalizations for skipping)
- YAGNI enforcement
- Approach comparison framework (2-3 options, lead with recommendation)
- Output: `docs/brainstorms/YYYY-MM-DD-<topic>-brainstorm.md`

**Takes from CE workflows:brainstorm:**
- Phase 0: Assess whether brainstorming is even needed (clear requirements → skip)
- Phase 1: Lightweight repo research before dialogue
- Phase 2: Collaborative dialogue — one question at a time, prefer multiple choice
- Phase 3: Capture document structure (What, Why, Key Decisions, Open Questions)
- Phase 4: Handoff options (refine, proceed to planning, ask more)
- "NEVER CODE" enforcement

**Strips:**
- CE's AskUserQuestion tool references (use platform-native questioning)
- CE's brainstorming skill reference (this IS the brainstorming skill)

**Key design decision:** When running autonomously (as a worker), skip interactive dialogue. Instead: research → analyze requirements → propose approaches → document decisions → flag open questions for escalation.

---

### Task 2: writing-plans

**Create:** `.claude/skills/writing-plans/SKILL.md`

**Takes from SP writing-plans:**
- Zero-context assumption: the implementer knows nothing about the codebase
- Bite-sized tasks (2-5 min each, one action per step)
- Exact file paths, complete code examples, test commands with expected output
- TDD structure per task (write failing test → verify fails → implement → verify passes → commit)
- DRY / YAGNI enforcement
- Parallelism annotation (independent vs sequential, dependency notation)

**Takes from CE workflows:plan:**
- Research decision logic: local research ALWAYS runs; external research is CONDITIONAL based on signals (topic risk, user familiarity, uncertainty level)
- Learnings integration: search `docs/solutions/` for past solutions that apply (gotchas, patterns)
- Detail level choice: Minimal (simple bugs) / Standard (most features) / Comprehensive (major features)
- Output format: `docs/plans/YYYY-MM-DD-<type>-<name>-plan.md` with YAML frontmatter

**Takes from CE deepen-plan (absorbed as optional enrichment phase):**
- After creating the base plan, optionally run parallel research agents per section
- Each agent enhances one section with best practices, edge cases, performance considerations
- Learnings from `docs/solutions/` are filtered to what's relevant

**Strips:**
- SP's `> For Claude: REQUIRED SUB-SKILL: Use superpowers:executing-plans` header (executing-plans is gone; workers use executing-work)
- SP's execution handoff (subagent-driven vs parallel session choice) — workers don't get this choice
- CE's Rails-specific examples, SpecFlow Analyzer, Figma references
- CE's issue creation (GitHub/Linear) — that's the controller's job

**Key design decision:** Plan header should reference `executing-work` for implementation. Parallelism annotations are required so the implementer can build a task graph.

---

### Task 3: deepening-plans

**Create:** `.claude/skills/deepening-plans/SKILL.md`

**Takes from CE deepen-plan:**
- Core concept: enhance an existing plan by running parallel research agents per section
- Each agent focuses on one aspect: best practices, edge cases, performance, security, testing
- Agents return findings as text; orchestrator synthesizes back into the plan
- Learnings from `docs/solutions/` are cross-referenced

**Strips:**
- CE's skill discovery mechanism (searching for all available skills)
- Rails-specific research agents (kieran-rails-reviewer, etc.)
- CE's `compound-engineering.local.md` config system

**Key design decision:** This is a lightweight optional skill. The heavy lifting is in writing-plans. This just adds a parallel research enhancement pass when the plan needs more depth.

---

### Task 4: executing-work

**Create:** `.claude/skills/executing-work/SKILL.md`

This is the most complex skill — it synthesizes CE workflows:work, SP subagent-driven-development, SP dispatching-parallel-agents, and CE swarm orchestration into a single skill with progressive disclosure.

**Takes from CE workflows:work:**
- Task execution loop: mark in_progress → read refs → match patterns → implement → test → mark done
- Incremental commits: commit when logical unit complete + tests pass, not on WIP
- Pattern matching: always read referenced files first, match existing conventions
- Quality check phase: full test suite + lint + type check before shipping

**Takes from SP subagent-driven-development:**
- Per-task dispatch: spawn fresh subagent per task for isolation
- Two-stage review: after each subagent completes, review its work before accepting
- Convergence detection: monitor task list, proceed when all complete

**Takes from SP dispatching-parallel-agents:**
- Domain identification: partition tasks by expertise area
- Focused prompts: each subagent gets only what it needs
- Result synthesis: combine parallel outputs, resolve conflicts

**Takes from CE swarm orchestration (the key enhancement):**
- Task graph with dependency edges (blockedBy)
- Atomic task claiming (prevents double-work)
- Parallel worker spawn: N subagents working independently, claiming tasks
- Convergence: when task_list shows all complete/cancelled, proceed

**Progressive disclosure structure:**
1. **Simple mode (default):** Sequential task execution. One task at a time. No sub-agents.
2. **Parallel mode (3+ independent tasks):** Spawn sub-agents per independent task group. Dependency-aware scheduling.
3. **Swarm mode (5+ tasks, explicitly requested):** Full swarm with task graph, atomic claiming, N concurrent workers.

**Critical constraint:** Sub-agents spawned by this skill MUST NOT themselves spawn further sub-agents. One level of delegation only. Enforce this by including "Do NOT spawn sub-agents or delegate to other agents" in every sub-agent prompt.

**Strips:**
- SP executing-plans' user feedback checkpoints (autonomous workers don't stop for feedback)
- CE work's git commands (VCS-agnostic — use `jj` or `git` based on project)
- CE work's Figma sync, screenshot capture, feature video
- CE work's PR template with Compound Engineered badge

**Key design decision:** The skill loads SP TDD and SP verification-before-completion as sub-skills. It doesn't duplicate their discipline — it integrates their invocation points into the execution loop.

---

### Task 5: document-review

**Create:** `.claude/skills/document-review/SKILL.md`

A single review skill that adapts to what it's reviewing — specs, plans, or implementation output. Plays triple duty across architect, planner, and implementer phases.

**Takes from CE document-review:**
- Structured self-review process: systematic line-by-line evaluation
- Check categories: completeness, clarity, consistency, actionability

**Enhancements beyond CE:**
- **Mode detection based on input:** Architect output → validate testable acceptance criteria, proper scoping. Plan → validate executable tasks, dependency annotations, no ambiguity. Implementation → validate spec compliance, test coverage, no missing pieces.
- **Cross-reference capability:** Check document against `docs/solutions/` for known patterns
- **Output:** List of findings by severity (blocking / should-fix / suggestion), not just prose

**Strips:**
- CE's generic "review this prose" framing — this is more structured and mode-aware

---

### Task 6: knowledge-capture

**Create:** `.claude/skills/knowledge-capture/SKILL.md`

Generalized version of CE's compound-docs + workflows:compound. The "compounding knowledge" system.

**Takes from CE workflows:compound:**
- Parallel subagent extraction: context analyzer, solution extractor, related docs finder, prevention strategist, category classifier — all run in parallel, return text
- Single-file output: orchestrator assembles one file from parallel results
- Category auto-detection from problem type

**Takes from CE compound-docs:**
- Output structure: `docs/solutions/{category}/{slug}.md` with YAML frontmatter
- Searchable categories: build-errors, test-failures, runtime-errors, performance-issues, etc.
- Cross-referencing with existing docs

**Strips:**
- CE's Rails-specific specialized agents (kieran-rails-reviewer, every-style-editor, etc.)
- CE's auto-invoke trigger phrases
- CE's "Compound Engineered" branding

**Two modes:**
- **Write mode (Retro):** Full capture — parallel agents extract and document a solved problem
- **Read mode (Architect/Planner):** Search `docs/solutions/` for relevant past learnings to inform current work

**Key design decision:** The YAML frontmatter schema should be simple and stable. Fields: `title`, `date`, `category`, `tags`, `related-issues`. No complex validation — let the content speak.

---

### Task 7: finishing-work

**Create:** `.claude/skills/finishing-work/SKILL.md`

**Takes from SP finishing-a-development-branch:**
- Structured decision: merge / PR / keep working / discard
- Pre-flight checks: tests pass, lint clean, no TODOs, diff review
- Evidence collection before claiming done

**Enhancements:**
- **Knowledge trigger:** After shipping, prompt "Was this non-trivial? If yes, invoke knowledge-capture."
- **VCS-agnostic:** No hardcoded git or jj commands — reference the project's VCS conventions

**Strips:**
- SP's interactive user choice (autonomous workers follow the workflow, not user prompts)
- SP's git-specific stash/branch cleanup commands

---

### Task 8: analyze

**Create:** `.claude/skills/analyze/SKILL.md`

**Takes from sjawhar analyze:**
- Parallel agent dispatch: type-checker, bug-finder, code-simplifier, code-reviewer run in parallel
- Conditional: test-analyzer only runs if tests were changed
- Severity categorization: critical / important / minor
- Structured output: findings list with file paths and line numbers

**Generalizes:**
- Remove jj-specific diff commands — use VCS-agnostic diff
- Remove hardcoded check commands — detect from project config (package.json scripts, Makefile, etc.)
- Make agent list configurable via progressive disclosure (default 4 agents, expandable)

---

## Cleanup Tasks

### Task 9: Disable irrelevant CE skills

Update the CE sync mechanism to exclude skills/commands that are dead weight:

**CE skills to disable** (move to commands or just exclude from symlink):
- `dhh-rails-style`, `andrew-kane-gem-writer`, `dspy-ruby`, `every-style-editor`
- `gemini-imagegen`, `rclone`
- `git-worktree` (use jj workspaces)
- `brainstorming` (replaced by our new brainstorming)
- `document-review` (replaced by our new document-review)

**CE commands to disable:**
- `test-xcode`, `deploy-docs`, `report-bug`, `feature-video`, `changelog`
- `lfg`, `slfg` (Legion IS the full pipeline)
- `orchestrating-swarms` (baked into executing-work)
- `resolve_parallel`, `resolve_pr_parallel`, `resolve_todo_parallel`
- `setup`, `triage`, `file-todos`
- `skill-creator`, `create-agent-skill`, `heal-skill` (use SP writing-skills)
- `generate_command`
- `agent-native-audit`
- `deepen-plan` (absorbed into writing-plans)

**CE to keep:**
- `workflows:brainstorm` (until our brainstorming skill is wired in)
- `workflows:plan` (until our writing-plans skill is wired in)
- `workflows:work` (until our executing-work skill is wired in)
- `workflows:review` (until our analyze + review workflow is wired in)
- `workflows:compound`, `compound-docs` (until our knowledge-capture skill is wired in)

### Task 10: Disable redundant SP skills

These SP skills are replaced by the new unified skills:

- `using-git-worktrees` (use jj workspaces)
- `dispatching-parallel-agents` (folded into executing-work)
- `subagent-driven-development` (folded into executing-work)
- `executing-plans` (replaced by executing-work; also stops for user feedback which breaks autonomous workers)
- `finishing-a-development-branch` (replaced by finishing-work)
- `requesting-code-review` (thin wrapper; replaced by analyze + review workflow)
- `brainstorming` (replaced by our new brainstorming)
- `writing-plans` (replaced by our new writing-plans)

---

## Orthogonal Investigation: Sub-Agent Swarm Affordance

**Not blocking this plan**, but must be investigated before executing-work can use swarm mode:

1. **Can a worker session dispatch sub-agent tasks?** What tool/API does it use? Does the OpenCode plugin's task system support this from within a worker?
2. **Is there recursion depth control?** Can we enforce "swarm agents must not spawn sub-swarms" at the plugin level, or only via prompt discipline?
3. **Task graph primitives:** Do `task_create`, `task_claim_next`, `task_update`, `task_list` actually exist in the current plugin? The implement.md workflow references them but they may be aspirational.

**If the plugin doesn't support this yet:** executing-work's swarm mode becomes a roadmap item. Simple and parallel modes still work via the existing `background_task` tool.

---

## Follow-Up (Not in This Plan)

After the skills are built:

1. **Update worker workflows** — Wire the new skills into architect.md, plan.md, implement.md, review.md, merge.md, replacing CE and old SP references
2. **Cross-family review mappings** — Define agent-type → model-family mappings per phase
3. **Test with a real issue** — Run a full architect→plan→implement→review→retro→merge cycle using the new skills
4. **Remove CE/SP transitional keeps** — Once new skills are validated, remove the CE workflow commands that were kept temporarily

---

## Priority Order

Build in this order — each skill builds on the previous:

| Order | Skill             | Why This Order                                              |
|:------|:------------------|:------------------------------------------------------------|
| 1     | document-review   | Dependency of architect + planner + implementer. Build first.|
| 2     | knowledge-capture | Dependency of retro + read-path for architect/planner.       |
| 3     | brainstorming     | Architect needs this. Relatively self-contained.             |
| 4     | writing-plans     | Planner needs this. Depends on knowledge-capture (read).     |
| 5     | analyze           | Implementer + reviewer need this. Self-contained.            |
| 6     | finishing-work    | Implementer ship phase. Triggers knowledge-capture.          |
| 7     | executing-work    | Most complex. Depends on all above. Build last.              |
| 8     | deepening-plans   | Optional enhancement. Lowest priority.                       |
| 9-10  | Cleanup tasks     | Disable irrelevant CE/SP after new skills are validated.     |
