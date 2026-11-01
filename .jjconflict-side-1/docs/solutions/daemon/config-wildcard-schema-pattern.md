---
title: "CONFIG_ANY_KEY wildcard for user-defined config sub-keys"
category: daemon
tags:
  - config
  - schema-validation
  - yaml
  - wildcard
date: 2026-04-12
status: active
module: daemon
related_issues:
  - "445"
symptoms:
  - "Unknown config key warning for valid installation owner names"
  - "collectUnknownKeys warns on arbitrary sub-keys"
---

# CONFIG_ANY_KEY Wildcard for User-Defined Config Sub-Keys

## Problem

The daemon's YAML config uses a schema-driven walker (`collectUnknownKeys`) to warn about
typos. When a config section has user-defined keys — like org names under `installations` —
the walker emits false-positive "Unknown config key" warnings for every valid entry.

```yaml
github_apps:
  implement:
    installations:
      acme: "111"       # ← walker warns: "Unknown config key: github_apps.implement.installations.acme"
      globex: "222"     # ← same
```

## Solution: Symbol-Based Wildcard Sentinel

Add a `CONFIG_ANY_KEY` Symbol to the schema interface:

```typescript
const CONFIG_ANY_KEY = Symbol("config-any-key");

interface ConfigSchema {
  [key: string]: ConfigSchema | null;
  [CONFIG_ANY_KEY]?: ConfigSchema | null;
}
```

In the schema definition, use `[CONFIG_ANY_KEY]: null` (or a sub-schema) to mark
sections that accept arbitrary keys:

```typescript
const CONFIG_SCHEMA: ConfigSchema = {
  github_apps: {
    implement: {
      app_id: null,
      private_key: null,
      installations: {
        [CONFIG_ANY_KEY]: null,  // any org name is valid here
      },
    },
  },
};
```

The walker checks `Object.hasOwn(schema, key)` first, then falls back to
`schema[CONFIG_ANY_KEY]`:

```typescript
function collectUnknownKeys(value, schema, pathParts, warnings) {
  for (const [key, childValue] of Object.entries(value)) {
    const childSchema = Object.hasOwn(schema, key)
      ? schema[key]
      : schema[CONFIG_ANY_KEY];  // wildcard fallback
    if (childSchema === undefined) {
      warnings.push(`Unknown config key: ${[...pathParts, key].join(".")}`);
      continue;
    }
    collectUnknownKeys(childValue, childSchema, [...pathParts, key], warnings);
  }
}
```

## Why Symbol, Not a String Sentinel

- A string like `"*"` would appear in `Object.entries()` and could collide with a
  real config key named `"*"`.
- Symbols are excluded from `Object.entries()` and `Object.keys()`, so the walker
  never tries to match `CONFIG_ANY_KEY` as a YAML key.
- Symbols are guaranteed unique — no collision risk.

## When to Use

Use this pattern whenever a config section has user-defined sub-keys:
- Per-org installation mappings
- Per-environment overrides
- Per-repo settings
- Any YAML map where the keys are data, not schema

## Validation of Sub-Key Values

Use `readStringRecord()` to validate that all values under a wildcard section
are strings:

```typescript
function readStringRecord(value: unknown, fieldPath: string): Record<string, string> | undefined {
  if (!isRecord(value)) throw new Error(`${fieldPath} must be a mapping`);
  const result: Record<string, string> = {};
  for (const [key, v] of Object.entries(value)) {
    if (typeof v !== "string") throw new Error(`${fieldPath}.${key} must be a string`);
    result[key] = v;
  }
  return result;
}
```

## Testing

Add an explicit test that arbitrary sub-keys produce no warnings:

```typescript
it("accepts arbitrary installation owner keys without unknown-key warnings", () => {
  const result = loadConfigFromFile(
    "github_apps:\n  implement:\n    app_id: x\n    private_key: y\n    installations:\n      acme-inc: i1\n      globex-labs: i2\n",
    "/tmp/x"
  );
  expect(result.warnings).toEqual([]);
});
```
