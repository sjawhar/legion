# Promoted Agents — Controller Role Discovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Enable the controller to discover a live `legion-po` role holder via Envoy and notify them of triage/dispatch decisions.

**Architecture:** Skill-only change to `.opencode/skills/legion-controller/SKILL.md`. No daemon code, no CLI commands, no TypeScript. The controller gains a per-iteration boolean check for a `legion-po` role holder using `envoy_sessions` (MCP tool), and advisory `envoy_publish` notifications to `notifications.role.legion-po`. Responses come through existing issue-comment flow. Repo-scoped roles, `promote` CLI, and persistent metadata are deferred to #518.

**Tech Stack:** Markdown (SKILL.md), Envoy MCP tools (`envoy_sessions`, `envoy_publish`)

**Assumptions:**
1. Scope is the PO's "immediate" deliverable only: controller checks for a global `legion-po` role holder. Repo-scoped roles and `promote` CLI are deferred to #518.
2. `envoy_role_set(role="legion-po")` adds `notifications.role.legion-po` to the session's topics array, making it discoverable via `envoy_sessions`.
3. Notifications are one-way and non-blocking. The PO responds via issue comments, picked up by the controller's existing feedback relay step (Step 2).
4. If no `legion-po` role holder exists, the controller proceeds unchanged.

**Variables already in scope in the controller's per-issue loop:**
- `$ISSUE_IDENTIFIER` — issue ID (e.g., `sjawhar-legion-509`)
- `$ISSUE_NUMBER` — issue number (e.g., `509`)
- `$ISSUE_REPO` — repo (e.g., `sjawhar/legion`)
- `$MODE` — worker mode derived from `ACTION_TO_MODE` (e.g., `implement`)

---

## Task 1: Add Domain Authority Discovery and Notification to Controller SKILL.md

**Dependency:** None (single task)

**Files:**
- Modify: `.opencode/skills/legion-controller/SKILL.md`
  - Insert new section after line 86 (end of "Autonomy vs Approval"), before line 88 ("## Envoy Notifications")
  - Insert notification paragraph in "### 4. Route Triage" after line 571
  - Insert notification paragraph in "### Dispatch (New Worker)" after line 701

### Part A: Add the Discovery Section

- [ ] **Step 1: Read lines 80-105 of `.opencode/skills/legion-controller/SKILL.md`**

Confirm: line 86 ends with "3. The situation is not covered by existing rules", line 87 is blank, line 88 starts "## Envoy Notifications".

- [ ] **Step 2: Insert the Domain Authority Discovery section between lines 87 and 88**

Insert after line 87 and before line 88:

```markdown
### Domain Authority Discovery

At the start of each loop iteration, check if a `legion-po` role holder exists by calling
`envoy_sessions` and scanning the result for a session whose `topics` array contains the
exact string `"notifications.role.legion-po"`. Store the result as a boolean (`HAS_PO`)
for the iteration.

If `envoy_sessions` fails or no session has the `legion-po` role topic, set `HAS_PO` to
false and skip all PO notifications for this iteration. Domain authority is advisory, never
blocking — the controller's existing logic is always the fallback.
```

- [ ] **Step 3: Verify the insertion**

Read lines 80-110 of `.opencode/skills/legion-controller/SKILL.md`. Confirm:
- The new section appears between "Autonomy vs Approval" and "## Envoy Notifications"
- No unclosed code blocks or broken markdown

### Part B: Add PO Notification to Triage Routing

- [ ] **Step 4: Read lines 552-575 of `.opencode/skills/legion-controller/SKILL.md`**

Confirm: "### 4. Route Triage" starts at line 552, the routing table guidance ends at line 571 ("When in doubt, route to Backlog."), and "### 5. Pull from Icebox" starts at line 573.

- [ ] **Step 5: Insert PO notification after line 571, before line 573**

Insert after "When in doubt, route to Backlog." (line 571):

```markdown

**PO notification:** If `HAS_PO` is true, after making the triage routing decision, notify:

```
envoy_publish(topic="notifications.role.legion-po", message="Triage: routing $ISSUE_IDENTIFIER to [Icebox|Backlog|Todo]. Override via issue comment.")
```

Replace `[Icebox|Backlog|Todo]` with the actual routing decision. Proceed immediately — do not
wait for a response. If `envoy_publish` fails, continue immediately — Envoy is advisory and the
controller's normal flow remains the source of truth. If the PO disagrees, they comment on the
issue, which the controller picks up via feedback relay (Step 2) on the next iteration.
```

- [ ] **Step 6: Verify the triage section edit**

Read lines 550-580. Confirm the notification paragraph is between the routing guidance and "### 5. Pull from Icebox".

### Part C: Add PO Notification to Dispatch

- [ ] **Step 7: Read lines 696-726 of `.opencode/skills/legion-controller/SKILL.md`**

Confirm: "### Dispatch (New Worker)" starts at line 696, the preamble about skill invocation ends at line 701 ("getting the full skill content.").

- [ ] **Step 8: Insert PO notification after line 701, before line 702**

Insert after "getting the full skill content." (line 701):

```markdown

**PO notification:** If `HAS_PO` is true, notify before dispatching:

```
envoy_publish(topic="notifications.role.legion-po", message="Dispatching $MODE worker for $ISSUE_IDENTIFIER in $ISSUE_REPO. Override via issue comment.")
```

Dispatch proceeds immediately — do not wait for a response. If `envoy_publish` fails, continue
immediately — Envoy is advisory and the controller's normal flow remains the source of truth.
```

- [ ] **Step 9: Verify the dispatch section edit**

Read lines 696-730. Confirm the notification paragraph appears before the dispatch code block.

### Part D: Verify and Commit

- [ ] **Step 10: Full SKILL.md integrity check**

Read the entire modified `.opencode/skills/legion-controller/SKILL.md` and verify:
- No unclosed code blocks
- No broken markdown tables
- The three insertions (discovery section, triage notification, dispatch notification) are each in the correct location
- All `envoy_publish(...)` calls use the established convention from the codebase

- [ ] **Step 11: Run repo checks**

Run: `bun test`
Expected: All tests pass (no TypeScript was modified)

Run: `bunx biome check src/ && bunx tsc --noEmit`
Expected: Clean (no TypeScript changes)

- [ ] **Step 12: Describe and advance**

```bash
jj describe -m "feat(controller-skill): discover legion-po role holder via envoy_sessions and notify before triage/dispatch"
jj new
```
