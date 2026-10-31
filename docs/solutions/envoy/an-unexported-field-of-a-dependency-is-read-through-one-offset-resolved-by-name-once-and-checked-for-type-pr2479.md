---
title: "An unexported field of a dependency is read through one offset resolved by name once and checked for type"
category: envoy
tags:
  - reflect
  - unsafe
  - ygo
  - performance
date: 2026-10-29
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2479"
---

# An unexported field of a dependency is read through one offset resolved by name once and checked for type

When code must read a field a dependency does not export:

- **Resolve the field once, by name**, with `sync.OnceValue` around
  `reflect.TypeFor[T]().FieldByName(name)`. Never call `reflect.Value.FieldByName` on each read:
  it walks the struct every call.
- **Check the field's type and the path to it.** Panic, naming the field, when the field is
  missing, has another type, or sits behind a pointer. Then a dependency upgrade that renames or
  moves it fails every test that reads it, instead of reading a wrong pointer.
- **Add up the offsets along the field's index path.** An embedded struct's `Offset` is relative to
  the struct that declares it, not to the outer type.
- **Expose one helper that returns the field's address.** Reads go through it, and a test that
  must install a value writes through the same helper rather than a second copy of the `unsafe`
  expression.
- The check runs at the first read, not at program start. State that in the doc comment, and
  make sure some test reads the field.

## Evidence

- `pmdoc/update.go:554` `yTextStartOffset` resolves ygo's `YText.abstractType.start` by name,
  checks it against `*crdt.Item`, panics at `:558` and `:566`, and sums the offsets along
  `field.Index`. `:547` `yTextStartField` returns the address, and `:542` `yTextStart` reads
  through it. All three item walks use it: `yTextDeltaInTransaction`, `readUnsharedText` and
  `AuthoredTextRuns`.
- `pmdoc/read_unshared_test.go` `itemsDoc` installs a hand-built item list with
  `*yTextStartField(text) = head`.
- A throwaway benchmark measured 345.7 ns/op for the old per-call `FieldByName` chain,
  36.19 ns/op for a cached `FieldByIndex` and 6.4 ns/op for the cached offset, all with
  0 allocations.
