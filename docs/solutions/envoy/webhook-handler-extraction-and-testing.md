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
```

In production the handler takes the listener's `*bus.Client`, which satisfies it directly, and
the CI store (`*cistore.Store`) as its `CIRecorder`:

```go
webhook.GitHubHandler(github.Secret, github.MentionTrigger, github.ReviewerAppID, d.client, d.ciStore)
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
if github := cfg.GitHub; github != nil {
    routes = append(routes, webhookRoute{"/webhook/github", func(d *listenerDeps) http.Handler {
        return webhook.GitHubHandler(github.Secret, github.MentionTrigger, github.ReviewerAppID, d.client, d.ciStore)
    }})
}
```

`webhookRoutes` (`cmd/listener/main.go`) lists the enabled routes. `nil` pointer = provider disabled. No boolean flags, no separate "enabled" field. The config parser validates required secrets at startup — missing secret when provider is enabled = fail-fast.

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

## Webhook routes during startup

Webhook handlers need NATS to publish, but NATS connects asynchronously after startup. main registers every enabled webhook path on one `startingGate` before the HTTP server starts, so a delivery during startup is answered `503 service starting` (GitHub retries it). Once NATS and every store are open, main builds each route's handler over the complete `listenerDeps` and opens the gate onto them, so a handler is only ever constructed with, and holds, dependencies that exist.
