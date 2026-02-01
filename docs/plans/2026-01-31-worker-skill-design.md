# Worker Skill Design: Compound Engineering vs Superpowers Analysis

**Date:** 2026-01-31
**Status:** Draft — ready for review
**Context:** Determining which development workflow (Compound Engineering or Superpowers) should guide Legion worker behavior

---

## Executive Summary

Legion workers need a hybrid approach: **Superpowers discipline** (TDD, verification, systematic debugging) combined with **Compound Engineering tooling** (parallel reviewers, autonomy hierarchy, Linear integration patterns).

Pure Compound Engineering is too lenient for autonomous workers (review is optional, TDD not enforced). Pure Superpowers assumes human checkpoints that workers can't use. The hybrid extracts the best of both.

---

## Background

### The Question

Legion workers receive a Linear issue and work autonomously until done. What development methodology should they follow?

Two candidate skill systems were evaluated:

1. **Compound Engineering** (`compound-engineering:workflows:*`)
2. **Superpowers** (`superpowers:*`)

### Evaluation Method

- Read skill definitions for both systems
- Analyzed philosophy, review requirements, TDD stance, subagent usage
- Cross-referenced with Legion's architectural constraints (autonomous, self-terminating, no human in loop)

---

## Skill System Comparison

### Philosophy

| Aspect | Compound Engineering | Superpowers |
|--------|---------------------|-------------|
| **Primary goal** | Ship features fast | Correctness through discipline |
| **Review stance** | Optional (complex/risky only) | Mandatory two-stage (spec + quality) |
| **TDD stance** | Mentioned, not enforced | Iron law — delete code written before tests |
| **Subagent model** | Parallel for independent items | Sequential with review loops |
| **Termination** | Promise tag (exact string match) | Human approval checkpoints |

### Compound Engineering: `workflows:work`

**Strengths:**
- Clear phase structure (Quick Start → Execute → Quality Check → Ship)
- TodoWrite tracking throughout
- Incremental commits at logical checkpoints
- Parallel reviewer agents available when needed

**Weaknesses for autonomous workers:**
- Review is explicitly optional: "Don't use [reviewer agents] by default"
- TDD not enforced — tests mentioned but not required before code
- Designed for human-supervised work ("Get user approval to proceed")
- Quality bar: "tests + linting + following patterns is sufficient"

**Key quote:**
> "For most features: tests + linting + following patterns is sufficient."

This is fine when humans catch mistakes. Autonomous workers have no safety net.

### Superpowers: `subagent-driven-development` + `test-driven-development`

**Strengths:**
- TDD is non-negotiable: "NO PRODUCTION CODE WITHOUT A FAILING TEST FIRST"
- Two-stage review mandatory: spec compliance first, then code quality
- Watching tests fail proves they test the right thing
- Clear verification checklist before claiming completion

**Weaknesses for autonomous workers:**
- Assumes human checkpoints: "Use superpowers:finishing-a-development-branch"
- No Linear integration
- Designed for interactive sessions with human approval gates

**Key quote:**
> "Violating the letter of the rules is violating the spirit of the rules."

This discipline is exactly what autonomous workers need — but the human checkpoints must be replaced.

---

## Legion Worker Requirements

From the architecture docs, workers must:

1. **Execute autonomously** — no human in loop during implementation
2. **Self-terminate cleanly** — exit when done, hooks handle cleanup
3. **Maintain quality** — tests must pass before PR
4. **Have clear completion criteria** — Linear issue → PR created → status "In Review"
5. **Handle blockers gracefully** — label + comment + exit (not hang)

### Gap Analysis

| Requirement | Compound Engineering | Superpowers | Hybrid |
|-------------|---------------------|-------------|--------|
| Autonomous execution | ⚠️ Assumes human approval | ⚠️ Human checkpoints | ✅ Self-review decides |
| Self-termination | ✅ Promise tag pattern | ❌ Waits for human | ✅ Linear status = done |
| Quality gates | ⚠️ Optional review | ✅ Mandatory review | ✅ Mandatory review |
| Clear completion | ⚠️ PR created | ⚠️ Human decides | ✅ PR + Linear update |
| Blocker handling | ❌ Not addressed | ❌ Not addressed | ✅ Label + exit pattern |

---

## Recommendation: Hybrid Approach

### Core Discipline (from Superpowers)

1. **TDD is mandatory**
   - Write failing test before implementation
   - Watch it fail (proves test is valid)
   - Write minimal code to pass
   - No exceptions without explicit override in issue

2. **Verification before completion**
   - All tests pass
   - Self-review confirms implementation matches issue description
   - Evidence (test output) required before claiming done

3. **Systematic debugging**
   - When tests fail, methodical investigation
   - No "try random fixes" — understand root cause first

### Tooling & Patterns (from Compound Engineering)

1. **Autonomy decision hierarchy**
   ```
   Research (WebSearch, docs, similar code)
        ↓
   Consult subagents (code-architect, ux-designer)
        ↓
   Spike implementations (build options, compare)
        ↓
   Make decision (pick one, document rationale)
        ↓
   Non-blocking question (comment, continue other work)
        ↓
   Blocking question (label, exit) — LAST RESORT
   ```

2. **Parallel reviewer agents** (when appropriate)
   - `code-simplicity-reviewer` for complex logic
   - `security-sentinel` for auth/permissions code
   - `performance-oracle` for hot paths

3. **Linear as completion signal**
   - Issue "In Progress" → worker active
   - Issue "In Review" → worker done, PR ready
   - Labels for state signaling (`user-input-needed`, etc.)

---

## Proposed Worker Flow

```
┌─────────────────────────────────────────────────────────────┐
│ 1. UNDERSTAND                                               │
│    └─ Fetch Linear issue (title, description, comments)     │
│    └─ Identify acceptance criteria                          │
└─────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────┐
│ 2. TDD CYCLE (per acceptance criterion)                     │
│    ┌─────────────────────────────────────────────────────┐  │
│    │ RED: Write failing test for criterion               │  │
│    │      └─ Run test, confirm it fails correctly        │  │
│    └─────────────────────────────────────────────────────┘  │
│                          ↓                                  │
│    ┌─────────────────────────────────────────────────────┐  │
│    │ GREEN: Write minimal code to pass                   │  │
│    │        └─ Run test, confirm it passes               │  │
│    └─────────────────────────────────────────────────────┘  │
│                          ↓                                  │
│    ┌─────────────────────────────────────────────────────┐  │
│    │ REFACTOR: Clean up, keep tests green                │  │
│    └─────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────┐
│ 3. VERIFY                                                   │
│    └─ Run full test suite (not just new tests)              │
│    └─ Run linting                                           │
│    └─ Self-review: Does implementation match issue?         │
│        ├─ No → iterate (back to step 2)                     │
│        ├─ Quality issues → fix, re-verify                   │
│        └─ Yes, quality good → continue                      │
└─────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────┐
│ 4. SHIP                                                     │
│    └─ jj describe -m "$ISSUE_ID: Description"               │
│    └─ jj git push --named "$ISSUE_ID"=@                     │
│    └─ gh pr create (or update existing)                     │
│    └─ linear issue update --status "In Review"              │
│    └─ linear issue comment with PR link                     │
└─────────────────────────────────────────────────────────────┘
                              ↓
┌─────────────────────────────────────────────────────────────┐
│ 5. EXIT                                                     │
│    └─ Worker terminates                                     │
│    └─ Hooks handle cleanup (workspace merge, etc.)          │
└─────────────────────────────────────────────────────────────┘

     ══════════════════════════════════════════════════════
     BLOCKER PATH (any step):
     └─ Add `user-input-needed` label to Linear issue
     └─ Comment explaining what's blocking
     └─ Exit immediately
     ══════════════════════════════════════════════════════
```

---

## Self-Review Decision Logic

Workers must decide "done" vs "blocked" without human input. Proposed logic:

```
Is the issue description clear enough to define acceptance criteria?
├─ No → BLOCKED (need clarification)
└─ Yes → continue

Can acceptance criteria be expressed as tests?
├─ No → BLOCKED (need testable requirements)
└─ Yes → continue

Do all tests pass?
├─ No → iterate (not blocked, just incomplete)
└─ Yes → continue

Does implementation match issue description (not more, not less)?
├─ More than requested → remove extras, re-verify
├─ Less than requested → iterate
└─ Exactly as requested → DONE

Are there obvious quality issues?
├─ Yes → fix, re-verify
└─ No → DONE
```

**Key insight:** "Blocked" means "cannot proceed without human input." Test failures, quality issues, and incomplete work are NOT blockers — they're normal iteration.

---

## Skill Structure

Proposed skill files:

```
~/.claude/skills/legion-worker/
├── SKILL.md              # Main worker skill (this design)
├── tdd-cycle.md          # TDD subprocess instructions
├── self-review.md        # Self-review decision logic
├── blocker-handling.md   # When and how to signal blockers
└── linear-integration.md # Linear status/label conventions
```

Or single file if simpler:

```
~/.claude/skills/legion-worker/
└── SKILL.md              # Everything in one file
```

---

## Open Questions

1. **Reviewer agents by default?**
   - Compound Engineering says no. Superpowers says yes (but sequential).
   - Proposal: Skip unless issue is tagged `needs-review` or touches auth/security.

2. **How strict on TDD?**
   - Superpowers says "delete code written before tests."
   - Autonomous workers can't undo — should they restart the workspace?
   - Proposal: Treat as strong guidance, not hard restart.

3. **What if tests are hard to write?**
   - Superpowers says "test hard = design unclear."
   - Workers might not have authority to redesign.
   - Proposal: Comment findings, continue with best-effort tests, flag for human review.

4. **Integration with existing `ralph-dev-execute`?**
   - Current skill is simpler (no TDD enforcement).
   - Replace entirely, or layer this on top?

---

## Next Steps

1. Review this design doc
2. Decide on open questions
3. Implement as skill in `~/.claude/skills/legion-worker/`
4. Test with a few Linear issues before full deployment

---

## References

- [Superpowers: Test-Driven Development](~/.dotfiles/.claude/plugins/cache/claude-plugins-official/superpowers/4.1.1/skills/test-driven-development/SKILL.md)
- [Superpowers: Subagent-Driven Development](~/.dotfiles/.claude/plugins/cache/claude-plugins-official/superpowers/4.1.1/skills/subagent-driven-development/SKILL.md)
- [Compound Engineering: workflows:work](compound-engineering plugin)
- [Ralph Wiggum Plugin README](https://github.com/anthropics/claude-code/blob/main/plugins/ralph-wiggum/README.md)
- [Legion MVP Implementation Plan](./2026-01-31-mvp-implementation-plan.md)
- [Legion Architecture Design](./2026-01-30-ralph-dev-swarm-design.md)
