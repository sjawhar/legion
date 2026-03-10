---
title: "Deterministic Session ID Generation with Parameter Threading"
category: general
tags:
  - session-management
  - deterministic-systems
  - parameter-threading
  - backward-compatibility
date: 2026-03-11
status: active
module: daemon
related_issues:
  - "84"
symptoms:
  - "session IDs not deterministic across runs"
  - "need to thread parameters through multiple layers"
  - "maintaining backward compatibility while adding new parameters"
---

# Deterministic Session ID Generation with Parameter Threading

## Problem

When building deterministic systems like Legion, session IDs must be reproducible for the same input parameters. Adding new parameters (like dispatch version) requires threading them through multiple architectural layers while maintaining backward compatibility.

## Solution Pattern

### Layered Parameter Threading

Use a clean data flow pattern: `CLI args → validation → daemon API → session computation`

```typescript
// CLI layer: Parse and validate user input
const version = args.version ? parseInt(args.version, 10) : undefined;
if (version !== undefined && (!Number.isInteger(version) || version < 0)) {
  throw new Error("Version must be a non-negative integer");
}

// Daemon layer: Validate API contract
function validateWorkerRequest(req: WorkerRequest) {
  if (req.version !== undefined) {
    if (!Number.isInteger(req.version) || !Number.isSafeInteger(req.version) || req.version < 0) {
      throw new Error("Version must be a non-negative safe integer");
    }
  }
}

// State layer: Use clean inputs for deterministic computation
function computeSessionId(params: SessionParams): string {
  const hashInput = {
    team: params.team,
    issue: params.issue,
    version: params.version, // undefined for backward compatibility
  };
  return createHash('sha256').update(JSON.stringify(hashInput)).digest('hex').slice(0, 16);
}
```

### Defense-in-Depth Validation

Each layer validates what it controls:
- **CLI**: String parsing, basic range validation
- **Daemon**: Type safety, integer validation, safe integer bounds
- **State**: Clean input assumptions for deterministic computation

### Contract Testing for Determinism

Test behavior guarantees, not implementation details:

```typescript
test('session IDs are deterministic for same inputs', () => {
  const params = { team: 'test', issue: 'issue-1', version: 1 };
  const id1 = computeSessionId(params);
  const id2 = computeSessionId(params);
  expect(id1).toBe(id2);
});

test('different versions produce different session IDs', () => {
  const base = { team: 'test', issue: 'issue-1' };
  const id1 = computeSessionId({ ...base, version: 1 });
  const id2 = computeSessionId({ ...base, version: 2 });
  expect(id1).not.toBe(id2);
});
```

## Key Patterns

1. **Optional Parameters with Defaults**: Enable backward compatibility by making new parameters optional
2. **Boundary Validation**: Validate at every layer boundary to prevent invalid state propagation
3. **Clean Data Flow**: Each layer passes validated, clean data to the next layer
4. **Comprehensive Test Coverage**: Test across all layers to catch integration issues

## Gotchas

- **JSON.stringify() ordering**: Ensure consistent object key ordering for deterministic hashing
- **Type coercion**: Be explicit about string→number conversion and validation
- **Safe integer bounds**: Use `Number.isSafeInteger()` for parameters that will be used in computations
- **Undefined vs null**: Be consistent about how optional parameters are represented

## When to Use

- Adding parameters to deterministic systems
- Threading data through multiple architectural layers
- Maintaining backward compatibility while extending functionality
- Building reproducible session/ID generation systems

## Related Patterns

- State machine parameter validation
- Multi-layer API design
- Deterministic system design