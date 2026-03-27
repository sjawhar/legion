# review.md — Evidence Section & Handoff Enforcement

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add evidence requirements to review summary comments and enforce handoff writes before completion signaling in `review.md`.

**Architecture:** Two changes to `review.md`: (1) add an Evidence section to the Section 3 PR comment template + a Common Mistakes entry, (2) add mandatory-attempt language to Section 4.5 handoff write + a pre-signal checklist to Section 6. All changes are mirrored to both `.opencode/skills/` and `.claude/skills/`.

**Tech Stack:** Markdown only. No code changes.

**Parent issue:** #120 (Skill audit: top 10 recurring user corrections)
**Architecture plan:** `docs/plans/2026-03-27-skill-audit-top10-corrections.md`

---

## File Structure

| Action | File | Responsibility |
|--------|------|---------------|
| Modify | `.opencode/skills/legion-worker/workflows/review.md` | Primary review workflow (Sections 3, 4.5, 6, Common Mistakes) |
| Modify | `.claude/skills/legion-worker/workflows/review.md` | Mirror of primary (must be identical) |

Both files are currently identical. All edits apply to the primary first, then the file is copied to the mirror location.

---

## Task 1: Add Evidence section to Section 3 PR comment template — Independent

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/review.md` — Section 3 "Post Summary Comment"

- [ ] **Step 1: Locate the Section 3 PR comment template**

Find the `gh pr comment` block in Section 3. The current template ends with:
```
---
[Detailed summary of key findings]"
```

- [ ] **Step 2: Insert Evidence section into the template**

Add the following block **inside** the `gh pr comment` body string, between the findings summary and the closing `"`:

```markdown
### Evidence
- CI status: [paste `gh pr checks` output]
- [For each P1/P2: code snippet showing the issue, or test output demonstrating the problem]
```

The full template after editing should look like:

````bash
gh pr comment "$LEGION_ISSUE_ID" --body "## Review Summary

**CRITICAL (P1):** N issues
**IMPORTANT (P2):** N issues
**MINOR (P3):** N suggestions

[Brief verdict: approved to merge / needs changes]

---
[Detailed summary of key findings]

### Evidence
- CI status: [paste \`gh pr checks\` output]
- [For each P1/P2: code snippet showing the issue, or test output demonstrating the problem]"
````

**Note:** Inside the bash string, backticks in `` `gh pr checks` `` must be escaped as `` \` `` to avoid shell interpretation.

- [ ] **Step 3: Verify the edit**

Read the modified section and confirm:
- The Evidence section is inside the `--body` string (before the closing `"`)
- CI status line references `gh pr checks`
- P1/P2 evidence requirement is present
- The rest of the template is unchanged

---

## Task 2: Add Common Mistakes entry for evidence — Independent

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/review.md` — Common Mistakes table

- [ ] **Step 1: Locate the Common Mistakes table**

Find the `## Common Mistakes` section at the end of the file. The current table has 5 rows.

- [ ] **Step 2: Append new row to the table**

Add this row at the end of the Common Mistakes table:

```markdown
| Posting review findings without evidence | Include code snippets, CI output, or reproduction steps for every P1/P2 finding. |
```

- [ ] **Step 3: Verify the edit**

Read the Common Mistakes table and confirm:
- New row is present as the last entry
- Wording matches exactly
- Table formatting is consistent with existing rows

---

## Task 3: Add handoff enforcement language to Section 4.5 — Independent

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/review.md` — Section 4.5 "Write Handoff Data"

- [ ] **Step 1: Locate Section 4.5**

Find the `### 4.5. Write Handoff Data` section. It currently starts with:
```
Write handoff data (non-blocking) — BEFORE setting PR draft status:
```

- [ ] **Step 2: Add mandatory-attempt paragraph**

Add the following paragraph **after** the field description bullet list (the bullets describing `critical`, `important`, `minor`, `verdict`, `keyFindings`):

```markdown
You MUST attempt the handoff write before setting PR draft status or signaling completion. The `|| true` ensures CLI failures don't block you, but skipping this step entirely is not acceptable. If the write fails, note it in your PR comment.
```

- [ ] **Step 3: Verify the edit**

Read Section 4.5 and confirm:
- "MUST attempt" language is present
- `|| true` is referenced as the non-blocking mechanism
- The distinction is clear: attempt mandatory, success non-blocking
- Existing code block and field descriptions are unchanged

---

## Task 4: Add pre-signal checklist to Section 6 — Depends on: Task 3

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/review.md` — Section 6 "Signal Completion"

- [ ] **Step 1: Locate Section 6**

Find the `### 6. Signal Completion` section. It currently starts with:
```
**CRITICAL: The `worker-done` label is how the controller knows you finished.**
```

- [ ] **Step 2: Insert pre-signal checklist**

Add the following checklist **after** the opening CRITICAL paragraph and **before** the GitHub/Linear code blocks:

```markdown
Before adding labels, verify:
1. Summary comment posted (step 3)
2. Line-level comments posted (step 4)
3. Handoff write attempted (step 4.5)
4. PR draft status set (step 5)

If any were skipped, go back and do them.
```

- [ ] **Step 3: Verify the edit**

Read Section 6 and confirm:
- 4-item checklist is present
- Items reference correct step numbers (3, 4, 4.5, 5)
- Checklist appears before the label commands
- Existing label code blocks are unchanged

---

## Task 5: Mirror to `.claude/skills/` and verify — Depends on: Task 1, Task 2, Task 3, Task 4

**Files:**
- Source: `.opencode/skills/legion-worker/workflows/review.md`
- Target: `.claude/skills/legion-worker/workflows/review.md`

- [ ] **Step 1: Copy the modified file**

```bash
cp .opencode/skills/legion-worker/workflows/review.md .claude/skills/legion-worker/workflows/review.md
```

- [ ] **Step 2: Verify files are identical**

```bash
diff .opencode/skills/legion-worker/workflows/review.md .claude/skills/legion-worker/workflows/review.md
```

Expected: No output (files identical).

- [ ] **Step 3: Describe and advance**

```bash
jj describe -m "skill: add evidence section and handoff enforcement to review workflow (#127)"
jj new
```

---

## Testing Plan

### Setup
No application setup needed — these are Markdown skill files with no runtime component.

### Health Check
Not applicable (no running services).

### Verification Steps

1. **Evidence section in template**
   - Action: Read Section 3 of `.opencode/skills/legion-worker/workflows/review.md`
   - Expected: `### Evidence` block inside the `gh pr comment` body string, with CI status line and P1/P2 evidence requirement
   - Tool: `grep` or file read

2. **CI status explicitly required**
   - Action: Search for "gh pr checks" in Section 3
   - Expected: `CI status: [paste \`gh pr checks\` output]` present inside the Evidence section
   - Tool: `grep`

3. **Common Mistakes entry**
   - Action: Read the Common Mistakes table
   - Expected: Row with "Posting review findings without evidence" in the Mistake column
   - Tool: File read

4. **Handoff enforcement in Section 4.5**
   - Action: Read Section 4.5
   - Expected: "You MUST attempt the handoff write" paragraph present, `|| true` referenced, clear mandatory-attempt vs non-blocking-success distinction
   - Tool: File read

5. **Pre-signal checklist in Section 6**
   - Action: Read Section 6
   - Expected: 4-item checklist (steps 3, 4, 4.5, 5) before the label commands, with "If any were skipped, go back and do them."
   - Tool: File read

6. **Dual directory mirroring**
   - Action: `diff .opencode/skills/legion-worker/workflows/review.md .claude/skills/legion-worker/workflows/review.md`
   - Expected: No output (files identical)
   - Tool: `diff`

7. **No code impact**
   - Action: `bunx tsc --noEmit && bun test`
   - Expected: Type check passes, all tests pass (changes are markdown-only)
   - Tool: CLI
