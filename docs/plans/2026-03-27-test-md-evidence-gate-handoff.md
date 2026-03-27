# test.md Evidence Gate & Handoff Enforcement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Strengthen evidence requirements from guidance to hard gate, and enforce handoff write attempts before signaling completion in the test workflow.

**Architecture:** Two sets of markdown changes to `test.md` in both `.opencode/skills/` and `.claude/skills/` directories: (1) evidence gate callout + template updates + new Common Mistakes entry, (2) handoff enforcement language + pre-signal checklist. Changes applied to `.opencode/` first, then mirrored to `.claude/` via file copy.

**Tech Stack:** Markdown only. No code changes.

**Source issue:** #126 (sub-issue of #120)

---

## File Structure

| File | Action |
|------|--------|
| `.opencode/skills/legion-worker/workflows/test.md` | Modify (primary) |
| `.claude/skills/legion-worker/workflows/test.md` | Mirror (identical copy after edits) |

Both files are currently identical (verified by `diff`). All edits target `.opencode/` first, then the file is copied to `.claude/`.

---

### Task 1: Evidence Gate Strengthening (Change 7) — Independent

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/test.md`

Locations (line numbers from current file):
- Section 5 "Execute Acceptance Criteria": after line 159 (`Do NOT accept "it looks like it works" — capture actual artifacts.`)
- Section 6 "Post Results to PR" GitHub template: line 185 (`| [criterion 1] | ✅/❌ | [output/screenshot] |`)
- Section 6 "Post Results to PR" Linear template: line 229 (same pattern)
- Common Mistakes table: after the last row (line ~310)

- [ ] **Step 1: Add EVIDENCE GATE callout to Section 5**

Insert immediately after the line `Do NOT accept "it looks like it works" — capture actual artifacts.` (line 159):

```markdown

**EVIDENCE GATE:** Every acceptance criterion in your PR comment MUST include at least one concrete artifact: screenshot, command output, log excerpt, or — for non-behavioral criteria verifiable only by reading code — "Verified by code inspection: [file:line]". A test result without evidence is not a valid test. One artifact may cover multiple related criteria if explicitly noted.

**What does NOT count as evidence:**
- Local file paths (e.g., `/tmp/screenshot.png`) — the reviewer cannot access your filesystem
- "It works" or "Verified" without an artifact
- Descriptions of what you saw without proof
```

- [ ] **Step 2: Update PR template — GitHub (Section 6)**

On the line containing `| [criterion 1] | ✅/❌ | [output/screenshot] |` in the GitHub template, change to:

```markdown
| [criterion 1] | ✅/❌ | [REQUIRED: screenshot/output/log/code-ref] |
```

- [ ] **Step 3: Update PR template — Linear (Section 6)**

On the line containing `| [criterion 1] | ✅/❌ | [output/screenshot] |` in the Linear template, make the same change:

```markdown
| [criterion 1] | ✅/❌ | [REQUIRED: screenshot/output/log/code-ref] |
```

- [ ] **Step 4: Add Common Mistakes entry**

Add the following row to the Common Mistakes table (after the last existing row, before the end of the file):

```markdown
| Posting results without evidence artifacts | Every criterion needs at least one: screenshot, command output, log, or code reference. "It works" is not evidence. Local file paths are not evidence — the reviewer can't access your filesystem. |
```

- [ ] **Step 5: Verify changes**

Run:
```bash
grep -n 'EVIDENCE GATE' .opencode/skills/legion-worker/workflows/test.md
grep -c 'REQUIRED: screenshot/output/log/code-ref' .opencode/skills/legion-worker/workflows/test.md
grep -n 'Posting results without evidence' .opencode/skills/legion-worker/workflows/test.md
grep -n 'Local file paths' .opencode/skills/legion-worker/workflows/test.md
```
Expected: First grep returns a match. Second grep returns `2` (both templates). Third and fourth greps return matches.

- [ ] **Step 6: Commit**

```bash
jj describe -m "docs(test.md): elevate evidence requirement from guidance to hard gate

Add EVIDENCE GATE callout to Section 5 with explicit prohibition of local
file paths. Update PR template placeholders to mark evidence as REQUIRED.
Add Common Mistakes entry for missing evidence artifacts.

Part of #126"
jj new
```

---

### Task 2: Test Handoff Enforcement (Change 8) — Depends on: Task 1

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/test.md`

Locations (line numbers shift after Task 1, so use content anchors):
- Section 6.5 "Write Handoff Data": after the `legion handoff write` code block's closing fence, before the `**Linear:**` line
- Section 7 "Signal Completion": after the CRITICAL callout, before "If all criteria pass:"

- [ ] **Step 1: Add MANDATORY ATTEMPT callout to Section 6.5**

Insert immediately after the closing ` ``` ` of the `legion handoff write` code block (the one containing `|| true`), before the `**Linear:**` line:

```markdown

**MANDATORY ATTEMPT:** You MUST attempt the handoff write before signaling completion. The `|| true` ensures CLI failures don't block you, but skipping this step entirely is not acceptable. If the write fails, note it in your PR comment. The handoff plumbing may not be fully operational yet (#124), but establishing the habit now ensures data flows when the plumbing is fixed.
```

- [ ] **Step 2: Add pre-signal checklist to Section 7**

Insert immediately after the line `the issue silently stalls. This is the MOST IMPORTANT step.`, before `**If all criteria pass:**`:

```markdown

**Pre-signal checklist — verify before adding labels:**
1. ✅ PR results posted (step 6) — did you comment on the PR with the results table?
2. ✅ Handoff write attempted (step 6.5) — did you run the `legion handoff write` command?

If either was skipped, go back and do it now. Do not add labels until both are done.
```

- [ ] **Step 3: Verify changes**

Run:
```bash
grep -n 'MANDATORY ATTEMPT' .opencode/skills/legion-worker/workflows/test.md
grep -n 'Pre-signal checklist' .opencode/skills/legion-worker/workflows/test.md
grep -n 'plumbing may not be fully operational' .opencode/skills/legion-worker/workflows/test.md
```
Expected: All 3 greps return matches.

- [ ] **Step 4: Commit**

```bash
jj describe -m "docs(test.md): enforce handoff write attempt before signaling completion

Add MANDATORY ATTEMPT callout to Section 6.5 acknowledging partially broken
plumbing (#124). Add pre-signal checklist to Section 7 requiring both PR
results and handoff write before label changes.

Part of #126"
jj new
```

---

### Task 3: Mirror to .claude/ and Verify — Depends on: Task 1, Task 2

**Files:**
- Source: `.opencode/skills/legion-worker/workflows/test.md`
- Target: `.claude/skills/legion-worker/workflows/test.md`

- [ ] **Step 1: Copy the file**

```bash
cp .opencode/skills/legion-worker/workflows/test.md .claude/skills/legion-worker/workflows/test.md
```

- [ ] **Step 2: Verify both copies are identical**

```bash
diff .opencode/skills/legion-worker/workflows/test.md .claude/skills/legion-worker/workflows/test.md
```
Expected: No output (files are identical).

- [ ] **Step 3: Final acceptance criteria check**

Run all verification commands against BOTH directories:

```bash
# Evidence Gate (Change 7)
for dir in .opencode .claude; do
  echo "=== $dir ==="
  grep -c 'EVIDENCE GATE' $dir/skills/legion-worker/workflows/test.md
  grep -c 'REQUIRED: screenshot/output/log/code-ref' $dir/skills/legion-worker/workflows/test.md
  grep -c 'Posting results without evidence' $dir/skills/legion-worker/workflows/test.md
  grep -c 'Local file paths' $dir/skills/legion-worker/workflows/test.md
  grep -c 'code inspection' $dir/skills/legion-worker/workflows/test.md
  grep -c 'multiple related criteria' $dir/skills/legion-worker/workflows/test.md
done

# Handoff Enforcement (Change 8)
for dir in .opencode .claude; do
  echo "=== $dir ==="
  grep -c 'MANDATORY ATTEMPT' $dir/skills/legion-worker/workflows/test.md
  grep -c 'Pre-signal checklist' $dir/skills/legion-worker/workflows/test.md
  grep -c 'plumbing may not be fully operational' $dir/skills/legion-worker/workflows/test.md
  grep -c '|| true' $dir/skills/legion-worker/workflows/test.md
done
```

Expected: All counts >= 1 in both directories. `|| true` count should be unchanged from before edits.

- [ ] **Step 4: Commit**

```bash
jj describe -m "chore: mirror test.md changes to .claude/skills/

Part of #126"
jj new
```

---

## Testing Plan

### Setup
No infrastructure needed — markdown-only changes. No build, no boot.

### Health Check
Not applicable.

### Verification Steps

For each acceptance criterion from issue #126:

1. **Bold evidence gate callout in Section 5**
   - Action: `grep -n 'EVIDENCE GATE' .opencode/skills/legion-worker/workflows/test.md`
   - Expected: Match found after "capture actual artifacts" line, in Section 5
   - Tool: grep

2. **PR template marks evidence as REQUIRED**
   - Action: `grep -c 'REQUIRED: screenshot/output/log/code-ref' .opencode/skills/legion-worker/workflows/test.md`
   - Expected: Returns `2` (GitHub template + Linear template)
   - Tool: grep

3. **Allows "Verified by code inspection: [file:line]" for non-behavioral criteria**
   - Action: `grep 'Verified by code inspection' .opencode/skills/legion-worker/workflows/test.md`
   - Expected: Match found in EVIDENCE GATE callout text
   - Tool: grep

4. **Allows one artifact for multiple related criteria if noted**
   - Action: `grep 'multiple related criteria' .opencode/skills/legion-worker/workflows/test.md`
   - Expected: Match found in EVIDENCE GATE callout text
   - Tool: grep

5. **New Common Mistakes entry**
   - Action: `grep 'Posting results without evidence' .opencode/skills/legion-worker/workflows/test.md`
   - Expected: Match found in Common Mistakes table
   - Tool: grep

6. **Explicitly prohibits local file paths as evidence**
   - Action: `grep -c 'Local file paths' .opencode/skills/legion-worker/workflows/test.md`
   - Expected: Returns >= 1 (EVIDENCE GATE section and/or Common Mistakes entry)
   - Tool: grep

7. **Section 6.5 uses "MUST attempt" language**
   - Action: `grep 'MUST attempt' .opencode/skills/legion-worker/workflows/test.md`
   - Expected: Match found in Section 6.5
   - Tool: grep

8. **Section 7 has pre-signal checklist referencing steps 6 and 6.5**
   - Action: `grep -A5 'Pre-signal checklist' .opencode/skills/legion-worker/workflows/test.md`
   - Expected: Shows checklist with "step 6" and "step 6.5" references
   - Tool: grep

9. **|| true preserved (CLI failures non-blocking)**
   - Action: `grep '|| true' .opencode/skills/legion-worker/workflows/test.md`
   - Expected: Still present in handoff write command (not removed)
   - Tool: grep

10. **Distinction clear: attempt mandatory, success non-blocking**
    - Action: `grep -A3 'MANDATORY ATTEMPT' .opencode/skills/legion-worker/workflows/test.md`
    - Expected: Contains both "MUST attempt" and "|| true ensures CLI failures don't block"
    - Tool: grep

11. **Acknowledges handoff plumbing may not be fully operational**
    - Action: `grep '#124' .opencode/skills/legion-worker/workflows/test.md`
    - Expected: Match found referencing the known plumbing issue
    - Tool: grep

12. **Both directories identical**
    - Action: `diff .opencode/skills/legion-worker/workflows/test.md .claude/skills/legion-worker/workflows/test.md`
    - Expected: No output (exit code 0)
    - Tool: diff

### Tools Needed
- `grep` for content verification
- `diff` for mirror verification
- No infrastructure, no browser, no API calls needed
