---
title: "Structured JSON Summary Patterns for Envoy Normalization"
category: envoy
tags:
  - envoy
  - go
  - json
  - normalization
  - tdd
  - testing
date: 2026-04-04
status: active
module: envoy
related_issues:
  - "sjawhar-legion-202"
symptoms:
  - "githubSummary returns plain text instead of JSON"
  - "payload_summary lacks structured data"
  - "adding new event type to Envoy normalization"
---

# Structured JSON Summary Patterns for Envoy Normalization

Learnings from rewriting `githubSummary()` to produce structured JSON, following the existing `slackSummary()` pattern.

## Sibling Function as Template

When adding a new variant of an existing function (e.g., "make `githubSummary()` produce JSON like `slackSummary()` does"), **read the sibling first and treat it as the spec**:

- `slackSummary()` already encoded design decisions: `map[string]string` for all fields, `json.Marshal` with `_` error discard, empty string for missing data, always-present keys
- Following these conventions meant zero design ambiguity — the implementation was purely mechanical
- The resulting code looks like it was written by the same person

**When to apply:** Any time you're adding a parallel function in the same file or module. Check for an existing sibling before designing from scratch.

## `map[string]string` + `json.Marshal` for Event-Specific JSON

The pattern for producing JSON with event-specific fields in Go:

```go
var data map[string]string

switch event {
case "issue_comment":
    data = map[string]string{
        "kind":   "comment",
        "action": action,
        "repo":   repo,
        // ... event-specific keys
    }
case "push":
    data = map[string]string{
        "kind": "push",
        "repo": repo,
        "ref":  ref,
    }
default:
    data = map[string]string{
        "kind":   "unknown",
        "action": action,
        "repo":   repo,
    }
}

out, _ := json.Marshal(data)
return string(out)
```

**Why `map[string]string` over structs:**
- Each event type has different keys — maps avoid N struct definitions
- Diff-friendly: each key is visible on its own line
- `json.Marshal` on `map[string]string` never errors, so `_` discard is safe
- Keys are deterministic (alphabetical per Go's `encoding/json`)

**Why empty string for missing data (not omitted keys):**
- Follows `nestedString()` convention which returns `""` for missing paths
- Consumers can check `parsed["key"]` without nil-checking first
- `number` must go through `githubNumber()` (not raw `fmt.Sprintf`) because `fmt.Sprintf("%v", nil)` returns `"<nil>"`, not `""`

## TDD Table-Driven Tests for JSON Contracts

Write the test table **before** the implementation. The table becomes the contract document:

```go
tests := []struct {
    name         string
    event        string
    body         map[string]any
    expectedKeys []string       // exact key set
    checkValues  map[string]string  // value assertions
}{...}
```

**Three validation layers** (each catches different bugs):
1. `json.Unmarshal` — catches broken serialization
2. `len(parsed) != len(expectedKeys)` — catches extra/missing keys
3. `checkValues` loop — catches wrong value extraction paths

The `len` check is especially valuable — it catches "I added a key I shouldn't have" bugs that value-only checks miss.

## Fixture Enrichment Safety

When enriching existing test fixtures with new fields:

- **Safe to add:** Fields that don't affect routing, mention detection, or envelope count (e.g., `comment.html_url`, `comment.user.login`, `review.state`)
- **Unsafe to add:** Fields that change behavior — `issue.number` (changes topic routing), `issue.pull_request` (changes parent_kind from "issue" to "pr"), `pull_request` map on review fixtures (changes envelope count)

The plan's safety table (explicitly listing safe/unsafe fields per test) prevented breakage. Always build this table before touching existing fixtures.

## Edit Tool Gotcha: Go Map Literal Brace Stripping

When using the edit tool to replace content inside Go map literals, the auto-corrector may strip closing braces (`},`) that it interprets as "boundary echo lines" duplicating adjacent surviving lines. This happened systematically on 7 of 8 fixture edits.

**Symptoms:** `go vet` reports `missing ',' in composite literal` after an edit that looked correct.

**Workaround:**
1. Run `go vet` immediately after every edit to Go map/struct literals
2. If braces were stripped, re-insert them with a targeted edit
3. For multi-level nested maps, consider editing the entire enclosing block rather than just inner content

## Unicode Truncation

Any string truncation that will be embedded in JSON must use `[]rune`, not byte slicing:

```go
func truncateBody(s string, maxChars int) string {
    runes := []rune(s)
    if len(runes) <= maxChars {
        return s
    }
    return string(runes[:maxChars])
}
```

The test suite should verify both rune count AND that the result round-trips through `json.Marshal`/`json.Unmarshal`. The emoji test case (`🔥` × 600, truncated to 500) is the canonical regression test.
