# Plan Workflow

Transform a Linear issue into a reviewed, executable implementation plan.

## Workflow

```dot
digraph plan_workflow {
    rankdir=TB;
    node [shape=box];

    fetch [label="1. Fetch Issue"];
    unclear [label="Requirements unclear?" shape=diamond];
    research_plan [label="2. /workflows:plan (autonomous)"];
    deepen [label="3. /deepen-plan"];
    review [label="4. /compound-engineering:plan_review"];
    passed [label="Review passed?" shape=diamond];
    executable [label="5. /superpowers:writing-plans"];
    post [label="6. Post to Linear"];
    complete [label="7. Signal Completion"];
    exit_unclear [label="Exit (user-input-needed)"];

    fetch -> unclear;
    unclear -> exit_unclear [label="yes: after oracle + assumptions exhausted"];
    unclear -> research_plan [label="no"];
    research_plan -> deepen;
    deepen -> review;
    review -> passed;
    passed -> research_plan [label="no: address feedback"];
    passed -> executable [label="yes"];
    executable -> post;
    post -> complete;
}
```

### 1. Fetch the Issue

```
mcp__linear__get_issue with id: $LINEAR_ISSUE_ID
```

The `$LINEAR_ISSUE_ID` environment variable is set by the controller when spawning this worker.

Extract:
- Title and description
- Comments with additional context
- Acceptance criteria if present

### 2. Invoke /workflows:plan (Autonomous)

Invoke `/workflows:plan` with this context:

```
You are running autonomously without user interaction.
Do NOT use AskUserQuestion. If requirements are unclear:

1. Invoke /oracle [specific question] for research-based guidance
2. Make reasonable assumptions and document them explicitly
3. Only escalate to user-input-needed if you truly cannot proceed

Feature description:
[Linear issue title + description + comments]
```

The skill handles:
- Local research (repo-research-analyst, learnings-researcher)
- Conditional external research (best-practices-researcher, framework-docs-researcher)
- SpecFlow analysis for edge cases
- Structured plan creation

**If the skill determines requirements are fundamentally unclear** (even after oracle + assumptions):
1. Add `user-input-needed` label via `mcp__linear__update_issue`
2. Post a comment via `mcp__linear__create_comment` explaining what needs clarification
3. Exit immediately - do NOT add `worker-done`

### 3. Invoke /deepen-plan

Invoke `/deepen-plan` on the plan file from step 2.

This enhances each section with:
- Parallel research agents
- Available skills matched to plan content
- Institutional learnings from `docs/solutions/`
- Review agents for comprehensive coverage

### 4. Review with /compound-engineering:plan_review

Invoke `/compound-engineering:plan_review` to validate the plan.

**Iterate until review passes:**
1. Read the review feedback
2. Address each issue identified (may re-invoke portions of step 2 or 3)
3. Re-invoke `/compound-engineering:plan_review`
4. Repeat until no blocking issues remain

**Max 3 iterations.** If still failing:
1. Add `user-input-needed` label with a comment summarizing unresolved issues
2. Exit without `worker-done`

### 5. Invoke /superpowers:writing-plans

Convert the approved plan into executable, bite-sized tasks.

This creates step-by-step implementation instructions with:
- Exact file paths
- Complete code examples
- Test commands with expected output
- Commit points

This is what the implement workflow will follow.

### 6. Post to Linear

Use `mcp__linear__create_comment` to post the **full executable plan** from step 5.

The complete `/superpowers:writing-plans` output goes directly into the Linear comment - all tasks, all code examples, all test commands.

### 7. Signal Completion

Add `worker-done` label to the Linear issue via `mcp__linear__update_issue`, then exit.

**CRITICAL:** Only add `worker-done` after successfully posting the plan. Never add this label if:
- Requirements were unclear and could not be resolved (use `user-input-needed` instead)
- Plan review failed and was not resolved
- Any step failed to complete

## Quick Reference

| Step | Action | Skill/Tool |
|------|--------|------------|
| Fetch | Get issue details | `mcp__linear__get_issue` |
| Research + Structure | Create plan | `/workflows:plan` (autonomous) |
| Enhance | Deepen with agents | `/deepen-plan` |
| Validate | Review plan | `/compound-engineering:plan_review` (iterate) |
| Executable | Bite-sized tasks | `/superpowers:writing-plans` |
| Post | Full plan to issue | `mcp__linear__create_comment` |
| Complete | Add done label | `mcp__linear__update_issue` |

## Autonomous Context Template

When invoking skills that normally use `AskUserQuestion`:

```
You are running autonomously without user interaction.
Do NOT use AskUserQuestion. If uncertain:

1. Invoke /oracle [specific question] - cheap research-based guidance
2. Make reasonable assumptions and document them
3. Only escalate to user-input-needed as absolute last resort

[rest of prompt]
```

## Common Mistakes

| Mistake | Correction |
|---------|------------|
| Adding `worker-done` when requirements unclear | Use `user-input-needed` label, exit without `worker-done` |
| Skipping /deepen-plan | Always run to enhance plan with research |
| Posting summary instead of full plan | Post complete executable plan from /superpowers:writing-plans |
| Asking user questions | Use oracle first, then assumptions, escalate only as last resort |
| Skipping plan review iteration | Always iterate until review passes or max 3 attempts |
