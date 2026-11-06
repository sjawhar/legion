---
title: "A lenient decode of a hot-path type is a function, not an UnmarshalJSON method"
category: envoy
tags:
  - go
  - encoding-json
  - performance
  - dispatch
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch/model
related_issues:
  - "sjawhar/legion#2483"
---

# A lenient decode of a hot-path type is a function, not an UnmarshalJSON method

- Giving a type an `UnmarshalJSON` method makes `encoding/json` scan each value once more before it
  calls the method, so every decode pays, well-formed values included. A fast path inside the
  method does not win it back.
- To tolerate a malformed stored field, write a package function: `json.Unmarshal` into the type
  first and return on success; only on a `*json.UnmarshalTypeError` whose `Field` is that field,
  decode again with the field shadowed as `json.RawMessage`. Call it at every site that decodes the
  stored value, and leave request-body and event-payload decodes alone.
- Measure the decode base against head with benchstat before you claim it is free.

## Evidence

- sjawhar/legion#2483 first made `model.Anchor` lenient with `UnmarshalJSON`, then added the fast
  path a review asked for. Benchstat at 0d6079ef against main, one well-formed anchor: 2.211 µs →
  3.340 µs, +51% (p=0.002), same bytes and allocations.
- `model.DecodeAnchor` (`model/model.go:83`, its doc comment at `:78` says why it is not a method)
  replaced it at the six stored-anchor decode sites (`docs/ask_row.go:39`, `docs/marks.go:473`,
  `:657`, `:801`, `docs/anchor_blocks.go:66`, `api/comment_queries.go:219`). At c9c5430b: 2.232 µs →
  2.340 µs, `~ (p=0.105)`; settlement's `recordedMarkRefs` over every replay document `~ (p=0.796)`.
- Other reasons against such a method are in
  `docs/solutions/envoy/embedded-struct-unmarshal-promotion.md` (promotion through embedding) and
  `docs/solutions/envoy/a-wire-shape-fix-in-a-types-unmarshaljson-misses-every-route-that-serves-the-stored-json-raw-INFRA-569.md`
  (routes that serve the stored JSON raw).
