---
title: "Subagent plan compliance: naming drift and skill snippet verification"
category: delegation
tags:
  - delegation
  - subagent
  - naming-conventions
  - skill-authoring
  - plan-compliance
date: 2026-04-11
status: active
module: worker
related_issues:
  - "#240"
symptoms:
  - "subagent used different names than the plan"
  - "disposition names don't match spec"
  - "skill snippet has wrong function signature"
  - "JSON output uses internal names instead of spec names"
---

# Subagent Plan Compliance: Naming Drift and Skill Snippet Verification

## Problem

When delegating implementation tasks to deep subagents with detailed plans containing exact code, two drift patterns emerged:

1. **Naming drift** — the plan specified disposition names `promote`/`review`/`stale`/`keep` and field names `learningPath`/`issueIdsInjected`/`helpfulIssues`. The subagents chose `accepted`/`needs_review`/`archived`/`rejected` and `path`/`issues`/`touchedPaths`. Functionally equivalent, but the spec vocabulary leaked as internal names in JSON output.

2. **Skill snippet signature mismatch** — the plan wrote a SKILL.md code snippet calling `deriveLegionIdFromWorkspaceDir(workspaceDir)` with 1 argument, but the actual function requires 3: `(workspaceDir, env, homeDir)`. The plan was written before implementation existed, so signatures were speculative.

## Rules

### Put naming requirements in MUST DO, not just in code examples

Subagents read the MUST DO section as hard constraints but treat code examples as reference material they can deviate from. If specific names matter (API field names, enum values, status strings), list them explicitly:

```
## MUST DO
- Use exactly these disposition names: "promote", "review", "stale", "keep"
- Use exactly these field names: "learningPath", "issueIdsInjected", "helpfulIssues"
```

Putting them only in code blocks invites the subagent to "improve" them.

### Verify skill snippet code after implementation, not during planning

Skill files (SKILL.md) that contain code calling implementation functions should be written or verified in the LAST task — after the implementation exists — so function signatures, parameter names, and import paths are known, not guessed.

In issue #240, the plan wrote step 6.5 of the retro SKILL.md as part of Task 4, but the function signatures were guessed from the plan's Task 1 code examples. The cross-family review caught the mismatch. The fix was simple (`process.env` and `os.homedir()` as additional args) but would have silently broken the retro data collection path.

### Map internal names to spec names at serialization boundaries

When internal disposition names differ from spec names, add a mapping function at the boundary where data becomes external (JSON output, human report, status mutations):

```typescript
// At the serialization boundary — never leak internal names
const SPEC_NAMES: Record<InternalDisposition, string> = {
  accepted: "promote",
  needs_review: "review",
  archived: "stale",
  rejected: "keep",
};
```

This way internal refactoring doesn't affect the external API contract.
