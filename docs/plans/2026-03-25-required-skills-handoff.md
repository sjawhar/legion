# Required Skills in Plan Handoff — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **Required skills for downstream workers:**
> - **implement**: `using-jj` — version control
> - **test**: (none beyond standard)
> - **review**: (none beyond standard)

**Goal:** Add a `requiredSkills` field to the plan handoff so downstream workers (implementer, tester, reviewer) receive project-specific skill names from the planner instead of independently rediscovering them.

**Architecture:** The planner already discovers project-specific skills in step 1.2. We formalize this discovery into a structured `requiredSkills` field in the plan handoff JSON, categorized by downstream phase (`implement`, `test`, `review`). Downstream workflows read this field on startup: if present, they invoke the listed skills and skip independent discovery; if absent, they fall back to their existing independent skill discovery.

**Tech Stack:** TypeScript (Bun), Zod validation, Markdown skill files

---

## File Map

| File | Action | Responsibility |
|------|--------|---------------|
| `packages/daemon/src/handoff/types.ts` | Modify | Add `RequiredSkills` interface, add field to `PlanHandoff` |
| `packages/daemon/src/handoff/schema.ts` | Modify | Add Zod schema for `requiredSkills`, add to `planSchema` |
| `packages/daemon/src/handoff/__tests__/ledger.test.ts` | Modify | Add roundtrip test for `requiredSkills` field |
| `.opencode/skills/legion-worker/workflows/plan.md` | Modify | Update steps 1.2, 5, 5.5 for skill categorization and handoff |
| `.opencode/skills/legion-worker/workflows/implement.md` | Modify | Add skill loading from plan handoff at step 1.6 |
| `.opencode/skills/legion-worker/workflows/test.md` | Modify | Add skill loading from plan handoff at step 1 |
| `.opencode/skills/legion-worker/workflows/review.md` | Modify | Add skill loading from plan handoff at step 1 |

---

### Task 1: Add `requiredSkills` to TypeScript types, Zod schema, and tests — Independent

**Files:**
- Modify: `packages/daemon/src/handoff/types.ts` (add interface, extend PlanHandoff)
- Modify: `packages/daemon/src/handoff/schema.ts` (add Zod schema to planSchema)
- Modify: `packages/daemon/src/handoff/__tests__/ledger.test.ts` (add test)

- [ ] **Step 1: Write the tests**

Add three new tests to `packages/daemon/src/handoff/__tests__/ledger.test.ts` after the existing "writes and reads phase handoffs" test (~line 46):

```typescript
it("writes and reads plan handoffs with requiredSkills", async () => {
  workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

  writePhaseHandoff(workspaceDir, "plan", {
    taskCount: 5,
    independentTasks: 3,
    requiredSkills: {
      implement: ["reskin-environment", "task-workflow"],
      test: ["smoke-testing"],
      review: ["design-review"],
    },
  });

  const all = readAllHandoffs(workspaceDir);
  expect(all.plan).not.toBeUndefined();
  expect(all.plan?.phase).toBe("plan");

  // Verify requiredSkills survived the roundtrip
  const plan = all.plan as Record<string, unknown>;
  expect(plan.requiredSkills).toEqual({
    implement: ["reskin-environment", "task-workflow"],
    test: ["smoke-testing"],
    review: ["design-review"],
  });
});

it("accepts plan handoffs with partial requiredSkills", async () => {
  workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

  writePhaseHandoff(workspaceDir, "plan", {
    taskCount: 2,
    requiredSkills: {
      implement: ["some-skill"],
    },
  });

  const all = readAllHandoffs(workspaceDir);
  const plan = all.plan as Record<string, unknown>;
  expect(plan.requiredSkills).toEqual({
    implement: ["some-skill"],
  });
});

it("accepts plan handoffs without requiredSkills (backward compat)", async () => {
  workspaceDir = await mkdtemp(path.join(os.tmpdir(), "legion-handoff-"));

  writePhaseHandoff(workspaceDir, "plan", {
    taskCount: 3,
    concerns: ["no skills needed"],
  });

  const all = readAllHandoffs(workspaceDir);
  expect(all.plan).not.toBeUndefined();
  const plan = all.plan as Record<string, unknown>;
  expect(plan.requiredSkills).toBeUndefined();
});
```

- [ ] **Step 2: Run tests — confirm type error**

Run: `bun test packages/daemon/src/handoff/__tests__/ledger.test.ts`

The tests may pass at runtime (`.passthrough()` allows extra fields), but `bunx tsc --noEmit` should show a type error because `requiredSkills` isn't in the `PlanHandoff` interface yet.

- [ ] **Step 3: Add RequiredSkills interface to types.ts**

In `packages/daemon/src/handoff/types.ts`, add after the `RoutingHints` interface (after line 9):

```typescript
export interface RequiredSkills {
  implement?: string[];
  test?: string[];
  review?: string[];
}
```

Then add the field to `PlanHandoff` (after `workflowRecommendation?: string;`, before the closing `}`):

```typescript
  requiredSkills?: RequiredSkills;
```

- [ ] **Step 4: Add Zod schema for requiredSkills in schema.ts**

In `packages/daemon/src/handoff/schema.ts`, add before the `planSchema` definition (before line 55):

```typescript
const requiredSkillsSchema = z
  .object({
    implement: z.array(z.string()).optional(),
    test: z.array(z.string()).optional(),
    review: z.array(z.string()).optional(),
  })
  .optional();
```

Then add the field to `planSchema` (after `workflowRecommendation`):

```typescript
  requiredSkills: requiredSkillsSchema,
```

- [ ] **Step 5: Run focused test**

Run: `bun test packages/daemon/src/handoff/__tests__/ledger.test.ts`
Expected: All tests PASS

- [ ] **Step 6: Commit**

```bash
jj describe -m "feat(handoff): add requiredSkills field to PlanHandoff type and schema

Add RequiredSkills interface and Zod schema for the plan handoff's
requiredSkills field, enabling planners to specify which skills each
downstream phase (implement, test, review) should load."
jj new
```

---

### Task 2: Update plan workflow to formalize skill categorization and include requiredSkills — Independent

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/plan.md` (steps 1.2, 5, 5.5)

- [ ] **Step 1: Update step 1.2 — formalize skill categorization output**

In `.opencode/skills/legion-worker/workflows/plan.md`, replace the content of step 1.2 (lines 62-71). The new content should be:

```markdown
### 1.2. Check for Project-Specific Skills

Before diving into planning, check if the repo has skills relevant to this work:

1. List available skills: look for project-specific skills beyond the standard Legion workflows
2. Read the issue title, description, and labels — do any skills match the domain? (e.g., a "reskin" issue might have a `/reskin-environment` skill)
3. If relevant skills exist, **categorize each by downstream phase**:
   - **implement**: Skills that describe how to build, create, or code something (build tools, frameworks, coding standards)
   - **test**: Skills that describe how to verify, test, or validate something (testing tools, QA procedures, smoke testing)
   - **review**: Skills that describe review criteria or quality standards (code review, security review, accessibility)
   - Skills may apply to multiple phases — include them in each applicable category
4. Include any relevant skill invocations in the testing plan (step 3) so downstream workers use them

Also check AGENTS.md and CLAUDE.md for project-specific conventions that should inform the plan.

**Output of this step:** A categorized skill list to carry forward to steps 5 and 5.5:

    Required Skills:
    - implement: [skill-name-1, skill-name-2]
    - test: [skill-name-3]
    - review: [skill-name-4]

If no relevant skills are found beyond standard Legion workflows, note "No project-specific skills identified" and omit the `requiredSkills` field from the handoff.
```

- [ ] **Step 2: Update step 5 — include Required Skills section in posted plan**

In step 5 ("Post to Issue"), find the paragraph (around line 298):
> The complete `/superpowers:writing-plans` output goes directly into the issue comment - all tasks, all code examples, all test commands.

Replace with:

```markdown
The complete `/superpowers:writing-plans` output goes directly into the issue comment — all tasks, all code examples, all test commands.

**If project-specific skills were identified in step 1.2**, append a "Required Skills" section to the posted plan:

    ## Required Skills

    The following project-specific skills should be loaded by downstream workers:

    | Phase | Skills |
    |-------|--------|
    | Implement | `skill-name-1`, `skill-name-2` |
    | Test | `skill-name-3` |
    | Review | `skill-name-4` |

    Workers: invoke these skills at the start of your workflow before beginning work.
    If a skill is unavailable in your environment, proceed without it.

This section is informational for human readers. The structured data is in the handoff (step 5.5).
```

- [ ] **Step 3: Update step 5.5 — add requiredSkills to handoff JSON**

In step 5.5 ("Write Handoff Data"), update the example JSON (around lines 305-319) to include `requiredSkills`:

Add after `"workflowRecommendation": "Standard pipeline — all phases needed"`:
```json
  "requiredSkills": {
    "implement": ["reskin-environment", "task-workflow"],
    "test": ["smoke-testing"],
    "review": ["design-review"]
  }
```

Then update the **Fields** section (around lines 322-328) to add:

```markdown
- `requiredSkills`: Per-phase skill arrays from step 1.2 (optional — omit if no project-specific skills found)
  - `implement`: Skills the implementer should invoke before coding
  - `test`: Skills the tester should invoke before verification
  - `review`: Skills the reviewer should invoke before review
```

- [ ] **Step 4: Commit**

```bash
jj describe -m "feat(plan-workflow): formalize skill categorization and add requiredSkills to handoff

Update plan workflow step 1.2 to categorize discovered skills by phase,
step 5 to include Required Skills section in posted plan, and step 5.5
to write requiredSkills in plan handoff JSON."
jj new
```

---

### Task 3: Update downstream workflows to read requiredSkills from plan handoff — Independent

**Files:**
- Modify: `.opencode/skills/legion-worker/workflows/implement.md` (step 1.6)
- Modify: `.opencode/skills/legion-worker/workflows/test.md` (step 1)
- Modify: `.opencode/skills/legion-worker/workflows/review.md` (step 1)

The same behavioral change applies to all three files: read `requiredSkills.<phase>` from the plan handoff and invoke those skills. If the field is present, use it (skip independent discovery for that phase). If absent, fall back to the existing independent skill discovery.

- [ ] **Step 1: Update implement workflow (implement.md)**

Find step 1.6 "Read Prior Handoffs (Advisory)" (around line 60-68). After the existing content about noting concerns and routing hints, add:

```markdown
**Skill loading from plan handoff:** If the plan handoff includes a `requiredSkills.implement` array, invoke each listed skill before proceeding to step 2. This front-loads skills the planner identified as relevant, and replaces the independent skill discovery in step 1.5 for this run.

    # Example: if plan handoff contains requiredSkills.implement: ["reskin-environment", "task-workflow"]
    # Invoke each skill:
    # /reskin-environment
    # /task-workflow

If `requiredSkills` is absent or the plan handoff is missing, proceed with step 1.5's independent skill discovery as the fallback (current behavior, no regression).
```

- [ ] **Step 2: Update test workflow (test.md)**

Find the paragraph about checking for repo-specific skills in step 1 (around line 39):
> Also check for repo-specific skills that may define domain-specific testing procedures...

After this paragraph, add:

```markdown
**Skill loading from plan handoff:** Read the plan handoff for pre-identified testing skills:

    legion handoff read --phase plan --workspace . 2>/dev/null || echo '{}'

If the plan handoff includes a `requiredSkills.test` array, invoke each listed skill before proceeding. This replaces the repo-specific skill check above for this run.

If `requiredSkills` is absent or the plan handoff is missing, rely on the repo-specific skill check above as the fallback (current behavior, no regression).
```

- [ ] **Step 3: Update review workflow (review.md)**

Find step 1 "Gather Context", after the handoff read block (around line 53, after "This is advisory."). Add:

```markdown
**Skill loading from plan handoff:** If the plan handoff includes a `requiredSkills.review` array, invoke each listed skill before proceeding to step 2. This replaces the manual skill check above for this run.

If `requiredSkills` is absent or the plan handoff is missing, rely on the manual skill check above as the fallback (current behavior, no regression).
```

- [ ] **Step 4: Commit**

```bash
jj describe -m "feat(downstream-workflows): load requiredSkills from plan handoff

Implement, test, and review workflows now read requiredSkills from the
plan handoff on startup. When present, the handoff skills replace
independent discovery. When absent, workers fall back to current behavior."
jj new
```

---

### Task 4: Final verification — Depends on: Task 1, Task 2, Task 3

**Files:** All modified files from Tasks 1-3

- [ ] **Step 1: Run full test suite**

Run: `bun test`
Expected: All tests pass (including new requiredSkills tests)

- [ ] **Step 2: Run type checks**

Run: `bunx tsc --noEmit`
Expected: No type errors

- [ ] **Step 3: Run linter**

Run: `bunx biome check packages/daemon/src/handoff/`
Expected: Clean

- [ ] **Step 4: Verify markdown files are well-formed**

For each modified workflow file, verify:
- `requiredSkills` instructions appear in the correct step
- Code blocks are properly closed (no unclosed triple-backtick blocks)
- No broken markdown formatting or missing step numbers

Specific grep checks:
```bash
grep -n "requiredSkills" .opencode/skills/legion-worker/workflows/plan.md
grep -n "requiredSkills" .opencode/skills/legion-worker/workflows/implement.md
grep -n "requiredSkills" .opencode/skills/legion-worker/workflows/test.md
grep -n "requiredSkills" .opencode/skills/legion-worker/workflows/review.md
```
Expected: Each file contains at least one reference to `requiredSkills`.

- [ ] **Step 5: Final commit (if any fixups needed)**

```bash
jj describe -m "chore: fixup lint/format issues from requiredSkills changes"
jj new
```

---

## Testing Plan

### Setup
- `bun install` (if not already done)
- No running infrastructure needed — this change is to skill markdown files and TypeScript types/schemas

### Health Check
- `bunx tsc --noEmit` returns exit code 0
- `bun test` returns exit code 0

### Verification Steps

1. **requiredSkills roundtrip in handoff**
   - Action: Run `bun test packages/daemon/src/handoff/__tests__/ledger.test.ts`
   - Expected: All tests pass, including new requiredSkills tests
   - Tool: Bun test runner

2. **Backward compatibility — plan handoff without requiredSkills**
   - Action: The "accepts plan handoffs without requiredSkills" test covers this
   - Expected: Plan handoffs without the field still validate and read correctly
   - Tool: Bun test runner

3. **Partial requiredSkills (only some phases)**
   - Action: The "accepts plan handoffs with partial requiredSkills" test covers this
   - Expected: Plan handoffs with only `implement` (no `test`/`review`) validate correctly
   - Tool: Bun test runner

4. **Type safety**
   - Action: Run `bunx tsc --noEmit`
   - Expected: No type errors — `RequiredSkills` interface is properly typed
   - Tool: TypeScript compiler

5. **Workflow markdown correctness**
   - Action: `grep -n "requiredSkills" .opencode/skills/legion-worker/workflows/{plan,implement,test,review}.md`
   - Expected: Each of the 4 workflow files contains at least one `requiredSkills` reference
   - Tool: grep

### Tools Needed
- Bun test runner (`bun test`)
- TypeScript compiler (`bunx tsc --noEmit`)
- Biome linter (`bunx biome check`)

## Dependency Graph

```
Task 1 (types + schema + tests) ──────────────┐
Task 2 (plan.md updates) ─────────────────────┤──► Task 4 (final verification)
Task 3 (downstream workflow updates) ──────────┘
```

Tasks 1–3 are all **Independent** — they modify separate files with no shared state. Task 4 depends on all of them.
