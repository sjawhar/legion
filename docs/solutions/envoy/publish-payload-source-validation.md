---
title: "Envoy publish payload source field is a closed enum"
category: envoy
tags:
  - envoy
  - contracts
  - publish
  - api-validation
  - plan-verification
date: 2026-04-11
status: active
module: envoy-plugin
related_issues:
  - "sjawhar-legion-418"
symptoms:
  - "source must be one of: agent, github, slack, whatsapp, ghostwispr"
  - "400 error when publishing to Envoy"
  - "envoy publish returns 400"
---

# Envoy publish payload source field is a closed enum

## Context

The Envoy `/v1/messages/publish` endpoint validates the `source` field against a closed
whitelist defined in `packages/envoy/internal/contracts/generated.go`. The allowed values are:

- `agent` (default when omitted)
- `github`
- `slack`
- `whatsapp`
- `ghostwispr`

Any other value (including seemingly reasonable ones like `"cli"`, `"worker"`, `"plugin"`)
causes a 400 rejection.

## The Pattern

When omitted or empty, `source` defaults to `"agent"` in the publish handler
(`packages/envoy/cmd/listener/main.go`). This is the correct approach for most agent-
and CLI-originated messages.

**Safe default: omit `source` entirely.** Only specify it when publishing on behalf of
an external system (GitHub webhook, Slack event, etc.) that has its own whitelisted value.

## How This Was Caught

A plan specified `source: "cli"` for a CLI publish command. This looked reasonable but
would have failed at runtime. The error was caught during cross-family review (Oracle
reviewing the implementation against the Envoy contracts), not by tests or type checking.

This illustrates a general pattern: **plans that include literal code snippets for API
payloads should be validated against the actual API contracts during implementation**, not
taken at face value. Static analysis and type checking won't catch semantic validation
rules like enum whitelists — only contract-aware review or runtime testing will.

## Guidance

- **CLI tools publishing to Envoy**: omit `source` (defaults to `"agent"`)
- **MCP tools publishing to Envoy**: omit `source` or use `"agent"` explicitly
- **New external bridges** (e.g., a Discord bridge): add the new source to the contracts
  whitelist in `packages/envoy/internal/contracts/generated.go` first
- **Plans specifying Envoy payloads**: reference `packages/contracts/` to verify field
  constraints before including payload shapes in the plan
