---
title: "Parallel Subagent Execution with Background Tasks"
date: 2026-02-01
category: skill-patterns
tags:
  - subagents
  - parallel-execution
  - background-tasks
  - workflow-design
  - skill-authoring
  - claude-code
module: legion-worker
component: workflows
symptoms:
  - "how to run subagents in parallel"
  - "background task execution in workflows"
  - "Task tool not found"
  - "parallel compound execution"
  - "fresh context subagent"
slug: parallel-subagent-background-execution
---

# Parallel Subagent Execution with Background Tasks

## Problem

When designing workflows that benefit from multiple perspectives (e.g., retrospectives), you want to run subagents in parallel where one has full context and another starts fresh. The challenge is correctly invoking background execution while both processes work simultaneously.

## Symptoms

- Workflow documentation references non-existent "Task tool"
- Subagents run sequentially instead of in parallel
- Fresh subagent inherits parent context unintentionally
- Waiting for one subagent before starting own work

## Root Cause

Skill documentation may reference tools or patterns that don't exist in the current toolset. The correct mechanism for background subagent execution is:

1. **Bash tool** with `run_in_background: true` parameter
2. **claude -p** command to spawn a fresh Claude instance with specific instructions
3. Parent continues own work while background task runs

The common mistake is referencing a "Task tool" that doesn't exist, or using patterns from other agent frameworks.

## Solution

### Correct Pattern: Background Bash with claude -p

```markdown
### Launch Background Subagent (Parallel)

Use Bash tool with `run_in_background: true` to spawn a fresh subagent:

\`\`\`bash
# run_in_background: true
claude -p "You are analyzing a completed PR to capture learnings.

Issue: $LINEAR_ISSUE_ID
PR: $PR_URL

1. Fetch the PR diff and description via gh pr view and gh pr diff
2. Invoke /compound-engineering:workflows:compound to document learnings
3. Write output to docs/solutions/ in the current directory

Focus on patterns that would help future implementations."
\`\`\`

The subagent has NO prior context - it discovers what was learned from the PR alone.

### Do Your Own Work (In Parallel)

While the subagent runs in background, do your own work immediately.
You will be notified when the background task completes.
```

### Key Elements

1. **run_in_background: true** - Indicated in comment or tool parameter
2. **claude -p** - Spawns fresh Claude with no inherited context
3. **Explicit instructions** - Subagent needs all context in the prompt
4. **Parallel work** - Parent immediately continues without waiting
5. **Notification** - System notifies when background task completes

### Incorrect Pattern (Do Not Use)

```markdown
# WRONG: Task tool doesn't exist
Task tool:
  description: "Fresh retro analysis"
  prompt: |
    Your instructions here...
  run_in_background: true
```

## When to Use Parallel Subagents

| Scenario | Use Parallel? | Reasoning |
|----------|---------------|-----------|
| Multiple independent analyses | Yes | Different perspectives valuable |
| Fresh vs. full context needed | Yes | Context-free discovers different things |
| Sequential dependencies | No | One output needed before next starts |
| Single straightforward task | No | Overhead not worth it |

## Benefits of Dual-Perspective Pattern

The retro workflow uses this pattern effectively:

1. **Full-context agent** (you): Knows what was hard, captures decisions not obvious from code
2. **Fresh-context subagent**: Discovers learnings purely from the PR diff and description

This dual perspective catches:
- Implicit assumptions you forgot to document
- Patterns visible to fresh eyes
- Things you adapted to and stopped noticing

## Prevention Checklist

- [ ] **Verify tool exists**: Check available tools before documenting usage
- [ ] **Use Bash for background**: `run_in_background: true` parameter
- [ ] **Fresh context via claude -p**: Spawns isolated instance
- [ ] **Include all needed context in prompt**: Subagent has no access to parent state
- [ ] **Continue work immediately**: Don't wait for background task
- [ ] **Note notification mechanism**: System alerts when complete

## Related

- superpowers:writing-skills - Skill authoring best practices
- compound-engineering:workflows:compound - Documentation workflow
- Legion worker design doc - Workflow architecture decisions
