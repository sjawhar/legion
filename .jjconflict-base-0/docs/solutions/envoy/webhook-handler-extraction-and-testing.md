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
    routes = append(routes, webhookRoute{"/webhook/github", func(client *bus.Client, ciStore *cistore.Store) http.Handler {
        return webhook.GitHubHandler(github.Secret, github.MentionTrigger, github.ReviewerAppID, client, ciStore)
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
| 10 | Publish failure | 503 (Slack retries the delivery; GitHub does not redeliver a failed one on its own) |

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

Webhook handlers need NATS to publish, but NATS connects asynchronously after startup. main registers every enabled webhook path on the webhook `startingGate` (`/v1` has its own) before the HTTP server starts, so a delivery before NATS and the CI store are open is answered `503 service starting`. GitHub records that delivery as failed and does not redeliver it on its own. Once NATS and the CI store are open (`openWebhooks`, `cmd/listener/main.go`), main builds each route's handler over the NATS client and the CI store, the only dependencies a webhook uses, and opens the webhook gate onto them, so a handler is only ever constructed with, and holds, dependencies that exist. The webhooks do not wait for the interest and session caches or the durable consumer's bind: during a rolling deploy that bind waits out the task being replaced, for as long as its deregistration and shutdown take, while the load balancer already sends the replacement webhooks.

Legion (the TypeScript daemon) recovers part of what a lost GitHub delivery carried. Its resync (`packages/daemon/src/daemon/resync.ts`, `reconcilePrs`) re-reads the head and check rollup of every open pull request it has registered, so a missed checks settlement or head change is repaired on the next resync. A worker's catch-up (`catchup.ts`, `workerCatchup`) lists the pull request's comments, review comments and reviews newer than that role's own latest commit or comment, but only when the daemon resumes the worker after a delivery exception, a death or a lost workspace; an ordinary wake carries no catch-up, and the catch-up restores no daemon state. Nothing recovers the rest: a missed `opened` leaves the pull request unregistered until a later `synchronize` (its next push) registers it, and resync reads only registered ones; a missed review is never recorded, because only the review webhook sets `reviewDecision`, so a missed approval's `pr-ready` never fires and a missed `changes_requested` never returns the issue to `in_progress`; a missed close or merge is never applied, because resync skips a pull request GitHub reports closed (`resync.ts:102`); and no resync step re-reads a branch push. The Go daemon (`packages/daemon-go`) recovers none of it: it never reads pull request, check or review state from GitHub. That state comes only from the listener's events through its durable GitHub consumer (`intake.OpenConsumers`), which replays what reached NATS while the daemon was down but never sees a delivery the listener refused; its boot reconcile reads Dispatch only (`workflowRuntime.reconcile`, `internal/daemon/workflow.go`), and outside App token minting nothing but the `legion threads` and `legion gh` commands calls the GitHub API. So a missed `opened`, head change, checks settlement, close or merge stays missed, and so does a missed review: `review_decision` is set only by `classify.ApplyReview` (`internal/classify/decisions.go`) from a review event, so a lost approval leaves `Engine.advanceApproved` (`internal/workflow/engine.go`) waiting with the issue in review, and a lost `changes_requested` never sends it back to implementing.
