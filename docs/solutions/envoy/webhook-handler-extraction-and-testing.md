---
title: "Webhook handler extraction and testing patterns"
category: envoy
tags:
  - webhook
  - handler-extraction
  - testing
  - publisher-interface
  - table-driven-tests
date: 2026-04-11
status: active
module: envoy
related_issues:
  - "427"
symptoms:
  - "webhook handler is untestable in package main"
  - "need to test signature verification without NATS"
  - "Slack timestamp verification fails in tests"
---

# Webhook handler extraction and testing patterns

Patterns for extracting HTTP webhook handlers from `cmd/*/main.go` into testable internal packages, with comprehensive test coverage.

## Handler extraction: the mechanical steps

1. **Change package**: `package main` → `package webhook`
2. **Export the constructor**: `webhookHandler` → `GhostWisprHandler`
3. **Replace local interface with shared one**: per-package `envelopePublisher` → shared `Publisher`
4. **Delete `main()`**: the binary entry point moves to the consolidated process

The handler constructor takes config + `Publisher` and returns `http.HandlerFunc`:

```go
func GhostWisprHandler(secret string, publisher Publisher) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // ... handler logic
    }
}
```

## Publisher interface: the testability seam

```go
type Publisher interface {
    Publish(contracts.Envelope) error
}

type PublisherFunc func(contracts.Envelope) error

func (f PublisherFunc) Publish(item contracts.Envelope) error {
    return f(item)
}
```

This is the `http.HandlerFunc` pattern applied to publishing. In production, wrap the NATS client:

```go
webhookPublisher := webhook.PublisherFunc(func(item contracts.Envelope) error {
    return deps.Load().client.Publish(item)
})
```

In tests, use a mock that records calls:

```go
type mockPublisher struct {
    published []contracts.Envelope
    err       error
}

func (m *mockPublisher) Publish(item contracts.Envelope) error {
    m.published = append(m.published, item)
    return m.err
}
```

## Config-gated route registration

```go
if webhookCfg.GitHub != nil {
    mux.Handle("/webhook/github", readinessGate(
        func() bool { return deps.Load() != nil },
        webhook.GitHubHandler(webhookCfg.GitHub.Secret, ...),
    ))
}
```

`nil` pointer = provider disabled. No boolean flags, no separate "enabled" field. The config parser validates required secrets at startup — missing secret when provider is enabled = fail-fast.

## Always `TrimSpace` env var reads

```go
secret := strings.TrimSpace(os.Getenv("ENVOY_GITHUB_WEBHOOK_SECRET"))
if secret == "" {
    return nil, fmt.Errorf("ENVOY_GITHUB_WEBHOOK_SECRET required when github enabled")
}
```

Whitespace-only secrets (`"  "`) pass `!= ""` but are functionally empty. Always trim before validating. Add regression tests for whitespace-only values.

## Canonical test matrix for webhook handlers

Table-driven tests with `httptest.NewRequest` + `httptest.NewRecorder`. Every handler should cover:

| # | Case | Expected |
|---|------|----------|
| 1 | Non-POST method | 200 (not 405 — providers send GET health checks) |
| 2 | Missing required headers | 400 |
| 3 | Invalid JSON body | 400 |
| 4 | Valid event | 200, envelope published |
| 5 | Unknown/skipped event type | 200, not published |
| 6 | Valid signature | 200 |
| 7 | Invalid signature | 401 |
| 8 | No secret configured | Skip verification, 200 |
| 9 | Event/payload type mismatch | 400 |
| 10 | Publish failure | 503 (provider will retry) |

### Provider-specific additions

**Slack:**
- `url_verification` challenge response BEFORE signature check (Slack sends it unsigned during app setup)
- Timestamp within 5-minute window — tests must use `time.Now()`, not static timestamps
- `event_callback` with empty `event_id` → no publish

**GitHub:**
- Mention fan-out: one comment → up to 3 envelopes (base + resource mention + repo mention)
- Bot sender filtering: check `sender.type == "Bot"`, skip comment events from bots
- Custom mention trigger via config (`@legion` default)

**Ghost Wispr:**
- Signing secret is optional (empty = skip verification)
- Event type normalization: dots → underscores, lowercased

## Slack timestamp gotcha

`verify.Slack()` checks that the request timestamp is within 5 minutes of `time.Now()`. Tests using hardcoded timestamps (e.g., `"1234567890"` from 2009) silently fail with 401.

```go
// WRONG: hardcoded timestamp from 2009
ts := "1234567890"

// CORRECT: use current time
nowTS := strconv.FormatInt(time.Now().Unix(), 10)
```

## readinessGate + deps.Load() interaction

Webhook handlers need NATS to publish, but NATS connects asynchronously after startup. The `readinessGate` middleware returns 503 until `deps.Load() != nil`. The `PublisherFunc` closure evaluates `deps.Load()` at request time (not registration time), so it's always current. This is safe because `readinessGate` prevents requests from reaching the handler before deps is initialized.
