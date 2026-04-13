---
title: "Model Fallback Chain Implementation Patterns"
category: delegation
tags:
  - model-fallback
  - retry-pattern
  - opencode-plugin
  - yagni
  - contract-tests
  - api-design
date: 2026-04-13
status: active
related_issues:
  - "275"
symptoms:
  - "model request fails, need to try alternative models"
  - "contract test arity check forces unused parameters"
  - "how to add fallback behavior without regressing existing path"
---

# Model Fallback Chain Implementation Patterns

## Context

The model fallback chain system (issue #275) adds sequential model retry when a request fails.
Several patterns emerged during implementation and review that apply beyond this specific feature.

## Key Patterns

### 1. Conditional Path for Optional Capability

When adding fallback behavior to an existing hot path, use a conditional branch to preserve
the exact original code path when the capability isn't configured:

```typescript
if (chain.fallbacks.length > 0) {
  // new: use fallback retry
  result = await createRetryWithFallback(chain, sendPrompt);
} else {
  // original: single-attempt, zero overhead
  result = await sendPrompt(chain.primary);
}
```

**Why:** The zero-fallback case executes the exact pre-existing code — no wrapper overhead,
no behavioral risk. Reviewers can verify the "no change" case by reading one branch.

### 2. Contract Tests Should Reflect Actual Design

The OMO replacement contract tests (`omo-replacement-matrix.test.ts`) check function arity
as a shape contract. When a parameter was removed during review (the unused `FallbackChainOptions`
on `getModelOverlay`), the correct fix was updating the contract test from `>= 3` to `>= 2`
— not adding a dead parameter to satisfy the test.

**Rule:** If a contract test doesn't match the actual design, **update the contract test**.
Never add dead code or unused parameters to satisfy a shape test. The contract exists to
document reality, not to constrain it.

### 3. Iterable-Based Chain Design

Using `Iterable<string>` for the fallback chain means the retry function is decoupled from
chain internals:

```typescript
// The retry function only needs iteration
for (const model of chain) {
  // try model...
}

// But the chain object exposes structure for inspection
chain.primary    // first model
chain.fallbacks  // remaining models (for conditional path above)
```

This is a useful pattern whenever you have an ordered collection that needs both iteration
and structural inspection.

### 4. Deduplication at Construction Time

`createModelFallbackChain` deduplicates the model list automatically (preserving first
occurrence). This prevents wasted retry attempts if config accidentally lists the same model
twice. Apply this pattern to any ordered config list where duplicates would cause redundant work.

### 5. Config Arrays Need Explicit Merge

Arrays don't merge well with spread (`{...base, ...override}` replaces the whole array).
For config fields that are arrays (like `fallbackModels`), use explicit nullish coalescing:

```typescript
fallbackModels: override.fallbackModels ?? base.fallbackModels
```

This preserves the base value when the override doesn't set the field, while allowing
a complete override when it does. Watch for this in any Zod-validated config with array fields.

## Review-Driven Simplification

The initial implementation had a `FallbackChainOptions` interface and an extra parameter on
`getModelOverlay`. Review correctly identified this as speculative API surface — the parameter
was accepted but never read. Removing it reduced the API surface, eliminated the unused type,
and simplified tests (24 → 23 tests, consolidating a test for the removed parameter).

**Takeaway:** When review flags an unused parameter or type, check whether any consumer actually
uses it. If not, remove it entirely rather than keeping it "for future use." The cost of
re-adding is trivial; the cost of maintaining dead code is ongoing confusion.
