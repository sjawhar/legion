---
title: "Prose Summaries and Structured Payloads for Envoy Normalization"
category: envoy
tags:
  - envoy
  - go
  - normalization
  - payload
  - tdd
  - testing
date: 2026-04-04
status: active
module: envoy
---

# Prose Summaries and Structured Payloads for Envoy Normalization

Envoy normalization separates the notification a reader scans from the data a reader may inspect.

## Envelope Contract

`payload_summary` is concise, one-line prose of at most 160 characters. It names the source event
and its important context without serializing a map or duplicating the full body.

`payload` contains the source-specific structure. GitHub, Slack, and Ghost Wispr normalizers encode
the event fields there; renderers present that structure once as `message`. Fields such as authors,
URLs, event state, and a capped source body belong in `payload`, not in `payload_summary`.

## Reuse a Sibling Function

When adding or changing a source normalizer, read its sibling summary and payload functions first.
For example, a new GitHub event should follow the same division already established by
`githubSummary()` and `githubPayload()`:

- Reuse the existing extraction helpers for nested values, subject-safe identifiers, capped bodies,
  and summary length.
- Put the brief human description in the summary function and the event fields in the payload
  function.
- Preserve the sibling's missing-value and body-capping behavior unless the source contract
  requires a real difference.

The sibling is the local specification for how a parallel event should look. Reusing it keeps
normalizers consistent and avoids inventing a second schema or truncation policy.

## Test the Reader-Facing Boundary

Tests for a normalizer should assert both parts of the envelope independently:

1. Assert the literal prose summary, including its one-line and length constraints.
2. Decode `payload` and assert the fields a reader or router needs.
3. Include a body-bearing fixture so the test proves that the body stays structured rather than
   being copied into the summary.

This catches the two important regressions: opaque JSON in `payload_summary`, and a useful event
whose details never reach `payload`.
