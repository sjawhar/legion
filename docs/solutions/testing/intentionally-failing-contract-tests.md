---
title: "Intentionally-Failing Contract Tests and CI Exclusion"
category: testing
tags:
  - contract-tests
  - ci
  - bun-test
  - omo-replacement
  - path-ignore-patterns
  - test-design
date: 2026-04-13
status: active
related_issues:
  - "270"
symptoms:
  - "contract tests designed to fail break CI"
  - "it.todo() produces exit code 0 instead of non-zero"
  - "tests must fail but CI must be green"
  - "bun test --path-ignore-patterns"
---

# Intentionally-Failing Contract Tests and CI Exclusion

## Context

When writing contract tests that define an acceptance gate (all tests must FAIL initially, proving
capability gaps), there is a fundamental tension: CI requires green builds, but the spec requires
non-zero exit with failing tests. This came up in the OMO replacement matrix (issue #270).

## Key Learnings

### 1. `it.todo()` Is Not a Failing Test

`it.todo()` marks a test as "not yet written" — it produces exit code 0 with "N todo, 0 fail."
This is semantically different from a test that proves a gap exists.

- **`it.todo()`** = placeholder = exit 0 = CI green = **no proof of gap**
- **`it()` with real assertion** = active test = exit 1 = **proves the gap**

For contract tests, always use active assertions (`expect(module.foo).toBeDefined()`) that will
fail when the feature is missing. Reserve `it.todo()` for tests you haven't written yet.

### 2. CI Exclusion via `--path-ignore-patterns`

Bun's test runner supports `--path-ignore-patterns` to exclude files from the default test run:

```yaml
# .github/workflows/pr-and-main.yaml
- name: Test opencode-plugin
  run: bun test --path-ignore-patterns '**/omo-replacement-matrix*'
```

This keeps contract tests runnable on demand (`bun test path/to/file.test.ts`) while excluding
them from CI. The exclusion is a glob pattern, not a label — if more contract test files are
added, each needs its own exclusion or the pattern needs to be broadened.

**Maintainability concern:** There is no automated mechanism to remove the exclusion once all tests
pass. Consider adding a comment in the workflow file noting when to remove it (e.g., "remove when
T6/T8/T10/T11/T16-T21 are complete").

**Better for future use:** Use a consistent naming convention like `*.contract.test.ts` or
`*.matrix.test.ts` so a single CI exclusion pattern covers the entire class.

### 3. Contract Test Design: Behavior Over API Surface

Contract tests should assert observable behavior, not specific API surface. Three patterns to
watch for:

| Anti-Pattern | Why It's Fragile | Better Approach |
|-------------|------------------|-----------------|
| `expect(Proto.getDepth).toBeDefined()` | Prescribes method name | Test behavior: "spawning at depth > N is rejected" |
| `Function.length` for arity checks | Default params, rest params, destructured params all affect `.length` | Call with options object, verify option is respected |
| Hardcoded role names (`"atlas"`) | Constrains implementation to exact strings | Test via a mapping layer or role lookup |

These were P2 findings in the review — non-blocking for this PR but important for implementers
of subsequent tasks who may need to adjust these tests.

### 4. The `asRecord()` Helper Pattern

For dynamic property access on typed modules without `any` casts:

```typescript
function asRecord(value: unknown): Record<string, unknown> {
  return value as Record<string, unknown>;
}

// Usage: check if an export exists without TypeScript errors
expect(asRecord(pluginIndex).discoverSkills).toBeDefined();
```

This is clean and reusable for any test asserting existence of not-yet-implemented exports.

## When to Apply

- Writing acceptance-gate tests that must fail initially
- Excluding known-failing test suites from CI without disabling them
- Designing contract tests for capability gaps in a multi-task migration
