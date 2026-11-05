---
title: "A counting loop that replaces an allocating definition is fuzzed against it and pinned at zero allocations"
category: testing
tags:
  - go
  - fuzz
  - allocation
  - utf16
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2455"
---

# A counting loop that replaces an allocating definition is fuzzed against it and pinned at zero allocations

When you replace a definition that allocates (`len(utf16.Encode([]rune(s)))`) with a loop that
counts:

- Keep the old expression in the test file as the reference, and write one `Fuzz…` test against
  it whose `f.Add` seeds are your edge cases. Plain `go test` runs every seed, so a separate table
  test over the same cases only repeats it.
- Pin the property with `testing.AllocsPerRun(100, …) == 0`, writing the result to a
  package-level variable so the compiler keeps the call.
- In Go, `for _, r := range s` decodes exactly as `[]rune(s)` does: each invalid byte is one
  U+FFFD, and neither yields a surrogate or a rune above U+10FFFF. So a string's UTF-16 length is
  its rune count plus one for each rune above U+FFFF. Do not use `utf16.RuneLen` for this: it
  returns -1 for a surrogate.
- Seed with invalid bytes, an encoded surrogate (`\xed\xa0\x80`), a sequence above U+10FFFF
  (`\xf4\x90\x80\x80`), truncated sequences, and the plane edges U+FFFF, U+10000, U+10FFFF.

## Evidence

- `pmdoc/position.go` `len16` on sjawhar/legion#2455; `position_test.go` holds `len16Cases`,
  `referenceLen16`, `FuzzLen16` and `TestLen16DoesNotAllocate`. Review round 1 deleted the table
  test the seeds repeated (thread PRRT_kwDORFy7ds6xKhst). The tester's mutation that counted a
  character above U+FFFF as one unit failed `FuzzLen16`'s seeds under plain `go test`.
