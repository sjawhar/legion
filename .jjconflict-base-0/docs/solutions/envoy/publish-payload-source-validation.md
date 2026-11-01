---
title: "Envoy message payload source field is a closed enum"
category: envoy
tags:
  - envoy
  - contracts
  - publish
  - send
  - api-validation
  - plan-verification
date: 2026-04-11
status: active
module: envoy-plugin
related_issues:
  - "sjawhar-legion-418"
symptoms:
  - "source must be one of: agent, human, envoy, github, slack, whatsapp, ghostwispr"
  - "400 error when sending or publishing to Envoy"
  - "envoy send or envoy publish returns 400"
---

# Envoy message payload source field is a closed enum

## Context

The Envoy `/v1/messages/send` and `/v1/messages/publish` endpoints validate
the `source` field against the envelope enum in
`packages/envoy/internal/contracts/generated.go`. The allowed values are:

- `agent` (default when omitted)
- `human`
- `envoy`
- `github`
- `slack`
- `whatsapp`
- `ghostwispr`

Any other value (including seemingly reasonable ones like `"cli"`, `"worker"`, `"plugin"`)
causes a 400 rejection.

## The Pattern

When omitted or empty, `source` defaults to `"agent"` in the listener handlers.

**Agent senders: omit `source`.** Set `source` to `"human"` when a person is the sender.

## How This Was Caught

A plan specified `source: "cli"` for a CLI publish command. This looked reasonable but
would have failed at runtime. The error was caught during cross-family review (Oracle
reviewing the implementation against the Envoy contracts), not by tests or type checking.

This illustrates a general pattern: **plans that include literal code snippets for API
payloads should be validated against the actual API contracts during implementation**, not
taken at face value. Static analysis and type checking won't catch semantic validation
rules like enum whitelists — only contract-aware review or runtime testing will.

## Guidance

- **CLI tools sending or publishing to Envoy**: omit `source` (defaults to `"agent"`)
- **MCP tools sending or publishing to Envoy**: omit `source` or use `"agent"` explicitly
- **New external bridges** (e.g., a Discord bridge): add the new source to the contracts
  whitelist in `packages/envoy/internal/contracts/generated.go` first
- **Plans specifying Envoy payloads**: reference `packages/contracts/` to verify field
  constraints before including payload shapes in the plan
