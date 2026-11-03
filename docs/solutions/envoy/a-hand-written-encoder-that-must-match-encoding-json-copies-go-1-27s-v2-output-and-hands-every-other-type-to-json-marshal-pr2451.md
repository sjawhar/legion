---
title: "A hand-written encoder that must match encoding/json copies Go 1.27's v2 output and hands every other type to json.Marshal"
category: envoy
tags:
  - encoding-json
  - jsonv2
  - byte-identity
  - tokens
  - oracle
  - fuzz
date: 2026-10-28
status: active
module: packages/envoy/internal/dispatch/pmdoc
related_issues:
  - "sjawhar/legion#2451"
---

# A hand-written encoder that must match encoding/json copies Go 1.27's v2 output and hands every other type to json.Marshal

- In Go 1.27 the jsonv2 experiment is on by default, so `encoding/json.Marshal` is
  `encoding/json/v2` under the v1 options. A hand-written appender that must give Marshal's bytes
  copies `AppendQuote` and `AppendFloat` from `$(go env GOROOT)/src/encoding/json/internal/jsonwire/encode.go`,
  not v1's `encode.go`. Where they differ from v1:
  - each byte of invalid UTF-8 becomes the raw bytes of U+FFFD, not the escape `\ufffd`;
  - `<`, `>`, `&` and control characters other than `\b \f \n \r \t` are `\u00xx` in lowercase
    hex, U+2028 and U+2029 are escaped, and DEL is written as it is;
  - a float64 is in exponent form below 1e-6 and from 1e21 in magnitude, with `e-07` cut to `e-7`,
    and `-0` stays `-0`.
  Read the GOROOT source of the toolchain `go.mod` pins before you write each case, because the
  next toolchain can change them.
- Write by hand only the closed set of types the canonical form yields (here `string`, `float64`
  after `int`/`int64`/`float32` are converted, `bool`, `nil`, `[]any`, `[]string`, `Attrs`,
  `map[string]any`). Append every other value as `json.Marshal(value)`'s bytes. A named map type
  such as `crdt.Attributes`, `[]byte`, `json.Number` and a struct then encode as they always did,
  and NaN, ±Inf and a `chan` return Marshal's own error value.
- Sort an object's keys by their raw Go strings (`strings.Compare`), as Marshal does. Keep the
  order in which the old code met values, so the error reported first stays the same one.
- Keep the value built whole and given to `json.Marshal` as the test oracle. Assert bytes, the
  error message and the `errors.As` class over a value table, and add a fuzz target seeded from it.
  Without the oracle, the hand-written copy has no check, and it can drift from Marshal.

## Evidence

- `pmdoc/token.go:283` `appendTokenString` and `:337` `appendTokenFloat` copy jsonwire. `:200`
  `object` sorts the keys, and `:228` `value` falls back to `json.Marshal`.
- The plan for the change asked for v1's behaviour ("each invalid UTF-8 byte as `\ufffd`"). The
  first implementation found that this toolchain's Marshal writes raw U+FFFD bytes. An encoder
  written to the plan would have changed every token whose text holds invalid UTF-8.
- `pmdoc/token_test.go:304` `TestAppendTokenJSONEncodesEveryValueAsEncodingJSON` puts each value
  in a node attribute and in a mark attribute. `:378` `FuzzTokenJSON` ran 1.14 M times. Both
  compare against `tokenRepresentation` (`pmdoc/node_token_test.go:101`).
- Tokens of all 1,200 LINUX specs were byte-identical at base and head (PR body,
  `E2E (implementer)`).
