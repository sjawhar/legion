---
name: legion-worker
description: Implement a Linear issue autonomously
---

# Legion Worker

You are a Worker implementing a single Linear issue. Focus on completing the task, then cleanly exit.

## Environment

Required environment variables:
- `LINEAR_ISSUE_ID` - the issue you're implementing (e.g., `ENG-21`)
- `WORKSPACE_DIR` - the jj workspace directory for this issue
- `LEGION_DIR` - the main Legion directory (for context)

**IMPORTANT**: All file operations must use absolute paths within `$WORKSPACE_DIR`.

## Workflow

### Step 1: Understand the task

Use the `mcp__linear__get_issue` tool with `id` set to `$LINEAR_ISSUE_ID`.

Read the issue title, description, and any comments carefully.

### Step 2: Implement the solution

Work in `$WORKSPACE_DIR`. Follow these principles:
- **TDD**: Write tests first when appropriate
- **Ask for help**: If truly blocked, report it and exit

Implementation loop:
1. Understand what needs to change
2. Write/modify code in `$WORKSPACE_DIR`
3. Run tests if applicable
4. If tests fail, fix and repeat
5. If tests pass, continue to next step

In jj, all file changes are automatically tracked. No staging required.

### Step 3: Finalize your change

Ensure all work from *this session* is consolidated into a single clean change with a clear description.

```bash
# Squash any changes created during this session into one
jj squash --repository "$WORKSPACE_DIR" 2>/dev/null || true

# Set the change description
jj describe -m "$LINEAR_ISSUE_ID: Brief description of what was done" --repository "$WORKSPACE_DIR"
```

**Note**: This ensures *your session's work* is one change. If this issue was worked on by a previous worker session, there may be multiple changes in the workspace history - that's fine.

### Step 4: Push and create PR

```bash
# Push the branch
jj git push --named "$LINEAR_ISSUE_ID"=@ --repository "$WORKSPACE_DIR"

# Create PR (or update if exists)
gh pr create --title "$LINEAR_ISSUE_ID: Title" --body "Closes $LINEAR_ISSUE_ID" --head "$LINEAR_ISSUE_ID" || \
  gh pr edit "$LINEAR_ISSUE_ID" --title "$LINEAR_ISSUE_ID: Title" --body "Closes $LINEAR_ISSUE_ID"
```

### Step 5: Update Linear

Use `mcp__linear__update_issue` with:
- `id`: The issue ID
- `state`: "In Review"

Then use `mcp__linear__create_comment` with:
- `issueId`: The issue ID
- `body`: "PR ready for review: [PR_URL]"

(Get the PR URL from `gh pr view --json url -q '.url'`)

### Step 6: Exit

After completing all steps, end your response. The Controller will handle cleanup.

## Handling Blockers

If you encounter a blocker you cannot resolve:

1. Use `mcp__linear__update_issue` to add `user-input-needed` label
2. Use `mcp__linear__create_comment` explaining the blocker
3. Exit

## Important Notes

- **One issue only**: You implement exactly `$LINEAR_ISSUE_ID`, nothing else.
- **Don't loop**: Complete the workflow once, then exit.
- **Your session = one change**: Consolidate all work you do into a single change.
- **jj auto-tracks**: Unlike git, jj automatically tracks all file changes.
