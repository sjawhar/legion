---
title: "A value a library consumes is validated with that library's own parser, read back whole"
category: integration-patterns
tags:
  - config-validation
  - check-config
  - cloud-sql
  - cloudsqlconn
  - parsers
date: 2026-10-25
status: active
module: packages/daemon/internal/ompsessions
related_issues:
  - "INFRA-271"
  - "sjawhar/legion#2179"
---

# A value a library consumes is validated with that library's own parser, read back whole

- **When the configuration names something a library parses (a connection name, a resource
  path, a URL form), check it with that library's exported parser, not with a rule written
  beside it.** A rule written beside the parser drifts both ways: it refuses forms the library
  takes and accepts forms the library then fails on at runtime, after `--check-config` passed.
- **Require the parsed parts to rebuild the whole input.** Many parsers match an unanchored
  pattern and silently use part of a longer string.
- **Refuse whitespace outright** when the library hands the value to a remote API that will
  refuse it.

## Evidence

- `ompsessions.ValidInstance` used to require three non-empty colon segments.
  - It refused a domain-scoped project, `example.com:example-project:us-central1:example-instance`.
    cloudsqlconn's `instance.ParseConnName` (pattern `([^:]+(:[^:]+)?):([^:]+):([^:]+)`) accepts
    that form.
  - It accepted a quoted ` example-project:us-central1:example-instance`. That value passed
    `--check-config` and then failed every generation start at the Admin API.
- The connector's pattern is unanchored, so `a:b:c:d:e` parses as project `a:b`, region `c`,
  name `d`.
- Fix `b80c9de6821a`: refuse any whitespace, run `instance.ParseConnName`, and require
  `Project()+":"+Region()+":"+Name()` to equal the input.
  `TestValidInstanceIsTheConnectorsRule` covers each case.
