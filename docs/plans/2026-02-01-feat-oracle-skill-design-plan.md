# Oracle Skill Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add oracle research sub-skill for workers and user-feedback relay to controller.

**Architecture:** Oracle is a worker workflow that searches docs/solutions/, codebase, and external docs before the worker escalates to the user. Controller handles `user-feedback-given` by prompting the blocked worker to read Linear comments.

**Tech Stack:** Claude Code skills (markdown), Linear MCP, tmux

---

## Task 1: Create Oracle Workflow

**Files:**
- Create: `skills/legion-worker/workflows/oracle.md`

**Step 1: Create the oracle workflow file**

```markdown
# Oracle Workflow

Research skill for answering questions before escalating to user.

## When to Use

Invoke `/oracle [question]` when:
- Unsure about existing patterns in the codebase
- Documentation might exist but location unknown
- Before using `AskUserQuestion` for something that might be documented

## Research Strategy

Search in this order until you find a clear answer:

### 1. Institutional Learnings

```bash
# Search docs/solutions/ for relevant learnings
Grep pattern: [keywords from question]
Path: docs/solutions/
```

Read any matching files. These contain solved problems and established patterns.

### 2. Codebase Patterns

```bash
# Search for similar implementations
Grep pattern: [relevant function/class names]
Path: src/
```

Look for existing code that solves similar problems.

### 3. External Documentation

If the question is about a framework or library:
- Use `mcp__plugin_compound-engineering_context7__resolve-library-id` to find the library
- Use `mcp__plugin_compound-engineering_context7__query-docs` to search documentation

For general best practices:
- Use `WebSearch` with specific technical queries

## Output

Return what you found:
- If found: Provide the answer with source references
- If not found: Say "I couldn't find enough information about [topic]"

The calling workflow decides whether to proceed with the answer or escalate to the user.

## Example

```
/oracle How does this codebase handle authentication?

Searching docs/solutions/... found auth-patterns.md
Searching src/... found src/auth/session.py

Answer: Based on docs/solutions/auth-patterns.md and src/auth/session.py,
authentication uses session-based auth with JWT tokens stored in httpOnly cookies.
The Session class at src/auth/session.py:45 handles token validation.
```
```

**Step 2: Verify file was created**

Run: `cat skills/legion-worker/workflows/oracle.md | head -20`
Expected: Shows the workflow header and "When to Use" section

**Step 3: Commit**

```bash
jj describe -m "feat(worker): add oracle research workflow"
```

---

## Task 2: Update Worker SKILL.md to Reference Oracle

**Files:**
- Modify: `skills/legion-worker/SKILL.md`

**Step 1: Add oracle to the mode routing table**

Add after the existing table (around line 32):

```markdown
## Research Sub-Skill

Before using `AskUserQuestion`, workers should invoke the oracle:

| Sub-Skill | Workflow | Purpose |
|-----------|----------|---------|
| `oracle` | @workflows/oracle.md | Research before escalating |

Usage: `/oracle [your question]`
```

**Step 2: Verify the change**

Run: `grep -A5 "Research Sub-Skill" skills/legion-worker/SKILL.md`
Expected: Shows the new section with oracle reference

**Step 3: Commit**

```bash
jj describe -m "feat(worker): reference oracle sub-skill in SKILL.md"
```

---

## Task 3: Add User Feedback Handling to Controller

**Files:**
- Modify: `skills/legion-controller/SKILL.md`

**Step 1: Add `relay_user_feedback` action to the process table**

In section "2. Process Issues", add to the action table (around line 80):

```markdown
| `relay_user_feedback` | Prompt blocked worker to read Linear comments |
```

**Step 2: Add relay implementation after "Resume worker" section**

Add after the "Resume worker" bash block (around line 114):

```markdown
**Relay user feedback:**
```bash
# When user-feedback-given label is present
ISSUE_ID="ENG-21"
ISSUE_ID_LOWER=$(echo "$ISSUE_ID" | tr '[:upper:]' '[:lower:]')
SESSION="legion-$LEGION_SHORT_ID-worker-$ISSUE_ID_LOWER"

# Prompt worker to read comments (safe - no user content injected)
tmux send-keys -t "$SESSION:main" -l "User answered on Linear. Read the comments on your issue."
tmux send-keys -t "$SESSION:main" Enter

# Remove the user-feedback-given label
mcp__linear__update_issue with:
  id: <issue_id>
  labelIds: [<all labels except user-feedback-given>]
```
```

**Step 3: Verify the changes**

Run: `grep -A10 "relay_user_feedback" skills/legion-controller/SKILL.md`
Expected: Shows the new action and implementation

**Step 4: Commit**

```bash
jj describe -m "feat(controller): add user feedback relay to blocked workers"
```

---

## Task 4: Update State Module for Relay Action

**Files:**
- Modify: `src/legion/state/types.py`
- Modify: `src/legion/state/decision.py`

**Step 1: Add action type**

In `src/legion/state/types.py`, find the `ActionType` literal and add:

```python
ActionType = Literal[
    "skip",
    "dispatch_planner",
    "dispatch_implementer",
    "dispatch_reviewer",
    "dispatch_finisher",
    "resume_implementer_for_changes",
    "resume_implementer_for_retro",
    "transition_to_in_progress",
    "transition_to_retro",
    "escalate_blocked",
    "relay_user_feedback",  # Add this
]
```

**Step 2: Add decision logic**

In `src/legion/state/decision.py`, find where `has_user_feedback` is checked and add relay logic:

```python
# If worker is blocked AND user has provided feedback, relay it
if data.is_blocked and data.has_user_feedback:
    return "relay_user_feedback"
```

**Step 3: Run tests**

Run: `uv run pytest tests/state/ -v`
Expected: All tests pass (or identify which tests need updating)

**Step 4: Commit**

```bash
jj describe -m "feat(state): add relay_user_feedback action"
```

---

## Task 5: Add Test for Relay Action

**Files:**
- Modify: `tests/state/test_decision.py` (or appropriate test file)

**Step 1: Write the test**

```python
def test_relay_user_feedback_when_blocked_with_feedback():
    """When worker is blocked and user gave feedback, relay it."""
    data = FetchedIssueData(
        issue_id="ENG-21",
        status="In Progress",
        labels=["user-input-needed", "user-feedback-given"],
        pr_is_draft=None,
        has_live_worker=True,
        is_blocked=True,
        blocked_question="Which approach should I use?",
        has_user_feedback=True,
        has_user_input_needed=True,
    )

    action = suggest_action(data)

    assert action == "relay_user_feedback"
```

**Step 2: Run the test**

Run: `uv run pytest tests/state/test_decision.py::test_relay_user_feedback_when_blocked_with_feedback -v`
Expected: PASS

**Step 3: Commit**

```bash
jj describe -m "test(state): add test for relay_user_feedback action"
```

---

## Task 6: Integration Test

**Step 1: Verify oracle workflow is loadable**

Run: `cat skills/legion-worker/workflows/oracle.md`
Expected: Full oracle workflow content

**Step 2: Verify controller references are consistent**

Run: `grep -r "user-feedback-given" skills/`
Expected: Shows controller SKILL.md with relay implementation

**Step 3: Verify state module**

Run: `uv run python -c "from legion.state.types import ActionType; print('relay_user_feedback' in ActionType.__args__)"`
Expected: `True`

**Step 4: Run full test suite**

Run: `uv run pytest -x -q`
Expected: All tests pass

**Step 5: Final commit**

```bash
jj describe -m "feat: oracle sub-skill and user feedback relay

- Add oracle.md workflow for research before escalation
- Update worker SKILL.md to reference oracle
- Add relay_user_feedback action to controller
- Add state machine support for relay action"
```

---

## Summary

| Task | Files | Description |
|------|-------|-------------|
| 1 | `workflows/oracle.md` | Create oracle research workflow |
| 2 | `SKILL.md` (worker) | Reference oracle sub-skill |
| 3 | `SKILL.md` (controller) | Add user feedback relay |
| 4 | `types.py`, `decision.py` | State machine for relay action |
| 5 | `test_decision.py` | Test for relay action |
| 6 | - | Integration verification |
