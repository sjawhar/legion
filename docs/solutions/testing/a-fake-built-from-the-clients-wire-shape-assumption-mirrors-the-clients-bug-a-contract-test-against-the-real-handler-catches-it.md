---
title: "A fake Dispatch server built from the client's own wire-shape assumption mirrors the client's bug — only a contract test against the real handler catches it"
date: 2026-09-27
category: testing
module: broker
problem_type: integration_issue
component: api_layer
symptoms:
  - "Every approval-required secret or launcher-credential request is denied on its first poll, even after a human approves it in Dispatch"
  - "GetAsk decodes GET /api/v1/asks/{id} into a bare Ask struct while Dispatch's real handler nests the ask under an ask wrapper key, so every field decodes to its zero value"
  - "CreateIssue is refused with 400 ACTOR_KIND because its request body names no session actor"
  - "The project's own 14-case end-to-end suite, and a separate live-binary verification pass, both stay green, because their hand-built fake Dispatch server encodes the same bare shape the client wrongly assumes"
root_cause: wrong_api
resolution_type: code_fix
severity: critical
tags:
  - wire-shape-drift
  - contract-testing
  - fake-server
  - json-unmarshal
  - dispatch
  - broker
---

# A Fake Dispatch Server Built From the Client's Own Wire-Shape Assumption Mirrors the Client's Bug

## Problem

AGENTC-833's secrets broker (`packages/envoy/internal/broker`) approves or denies a secret
request by polling Dispatch for the answer to an ask it opened. `dispatch.Client.GetAsk`
(`packages/envoy/internal/broker/dispatch/client.go`) decoded `GET /api/v1/asks/{id}` straight
into a bare `Ask` struct for the whole implementation. Dispatch's real handler
(`packages/envoy/internal/dispatch/api/asks.go`'s `getAsk`, lines 688-693) never returns a bare
ask: it wraps the response as `{"ask": {...}, "replies": [...], "edits": [...], "followers":
[...]}`. Go's `encoding/json` silently ignores JSON keys a destination struct has no field for,
so decoding that envelope into a bare `Ask` produced an all-zero-value struct — every real call
read back an empty `ID`, `State`, and `Answer`, no error raised.

Downstream, `dispatch.Verdict` (`packages/envoy/internal/broker/dispatch/verdict.go:34-35`)
treats an ask whose ID does not match the one requested as `Denied`, with reason "ask id does
not match the request's own ask" — exactly the shape a zero-valued `Ask{ID: ""}` produces. The
fix commit's own words: "GetAsk decoded GET /api/v1/asks/{id} as a bare ask, but Dispatch nests
the ask under 'ask'; every read came back zero-valued and ApplyAnswer/applyAsk denied every
approval-required request on the first poll." In production this would have silently denied
every human-approved secret grant and every launcher-credential request before a human's answer
could ever reach the decision — the broker's entire approval path was inert. The same pass also
found `CreateIssue` sent no `actor` field, which Dispatch refuses from a bearer-authenticated
caller with `400 ACTOR_KIND`.

## Symptoms

- Every approval-required secret or launcher-credential request is denied on its first poll,
  even after a human approves it in Dispatch.
- `GetAsk` decodes `GET /api/v1/asks/{id}` into a bare `Ask` struct while Dispatch's real handler
  nests the ask under an `ask` wrapper key, so every field decodes to its zero value.
- `CreateIssue` is refused with `400 ACTOR_KIND` because its request body names no session actor.
- The project's own 14-case end-to-end suite, and a separate live-binary verification pass, both
  stay green, because their hand-built fake Dispatch server encodes the same bare shape the
  client wrongly assumes.

## What Didn't Work

Nothing in the implementation's own testing surface caught this, because every layer of testing
derived its expectations from the client's own types rather than from Dispatch's actual
serialization code:

- Task-scoped code review of the client and of every caller that reads a `dispatch.Ask` — the
  client compiled, its unit tests passed, and its shape looked internally consistent.
- The committed end-to-end suite (`packages/envoy/internal/broker/e2e/e2e_test.go`, 14 cases run
  as one continuous scenario against a real Postgres-backed store and a fake Dispatch
  `httptest.Server`) passed, because its fake handler for `GET /api/v1/asks/{id}` encoded
  `dispatch.Ask{ID: a.id, State: a.state, EditedAt: a.editedAt, Answer: a.answer}` directly on
  the wire — the exact bare shape the client's `GetAsk` expected, and exactly wrong.
- A dedicated live-surface verification pass that booted the compiled broker and CLI binaries
  against a hand-built fake Dispatch server also passed, for the same reason: whoever wrote that
  fake read the client's `Ask`/`Answer` types to decide what JSON to emit, not Dispatch's `getAsk`
  handler.

A fake built by reading the consumer's own struct, instead of the producer's actual
serialization code, cannot fail the consumer against its own bug — it was written to agree with
that bug.

## Solution

`GetAsk` now decodes the real envelope and refuses a response naming a different ask instead of
silently trusting a zero-value success (`packages/envoy/internal/broker/dispatch/client.go:127-142`):

```go
// GetAsk reads one ask. Dispatch answers GET /api/v1/asks/{id} with the ask nested under "ask"
// beside its replies, edits and followers; an answer naming any other ask is refused rather than
// trusted, so a zero or foreign ask can never reach a caller deciding a request on it.
func (c *Client) GetAsk(ctx context.Context, id string) (Ask, error) {
	var body struct {
		Ask Ask `json:"ask"`
	}
	path := "/api/v1/asks/" + url.PathEscape(id)
	if err := c.do(ctx, http.MethodGet, path, c.token, nil, &body); err != nil {
		return Ask{}, err
	}
	if body.Ask.ID != id {
		return Ask{}, fmt.Errorf("dispatch GET %s: response names ask %q", path, body.Ask.ID)
	}
	return body.Ask, nil
}
```

That matches the real handler's own response shape
(`packages/envoy/internal/dispatch/api/asks.go:688-693`):

```go
WriteJSON(w, http.StatusOK, struct {
    Ask       model.Ask           `json:"ask"`
    Replies   []model.Comment     `json:"replies"`
    Edits     []model.AskEdit     `json:"edits"`
    Followers []model.AskFollower `json:"followers"`
}{Ask: ask, Replies: replies, Edits: edits, Followers: followers})
```

The same change also fixed the e2e suite's fake to stop mirroring the bug
(`packages/envoy/internal/broker/e2e/e2e_test.go`, current tree) — it now wraps its response the
same way the real handler does, with the comment naming why:

```go
// Dispatch's own GET /api/v1/asks/{id} shape: the ask nested under "ask" beside its
// replies, edits and followers (internal/dispatch/api's getAsk).
json.NewEncoder(w).Encode(map[string]any{
    "ask":       dispatch.Ask{ID: a.id, State: a.state, EditedAt: a.editedAt, Answer: a.answer},
    "replies":   []any{},
    "edits":     []any{},
    "followers": []any{},
})
```

`CreateAsk` and `CreateIssue` now share one typed `actor` field
(`packages/envoy/internal/broker/dispatch/client.go`'s `brokerActor`) so every bearer-authenticated
write names a session actor, closing the second bug (`400 ACTOR_KIND`) the same pass found.

The permanent regression test,
`packages/envoy/internal/broker/dispatch/client_contract_test.go`, does not add another fake. It
runs the broker's own `Client` against Dispatch's real HTTP handler — `packages/envoy/internal/dispatch/api`'s
`Register` function (`server.go:190`), wired through that same package's own `NewDeps` — on a
migrated Postgres database (`internal/dispatch/store/storetest`), for every route the client
calls — `CreateAsk`, `GetAsk`, `CreateIssue`, `ListIssues`, `Whoami`, `RetractAsk` — so the
client's reading of the wire is checked against the real serialization code, not against
anyone's re-derivation of it.

## Why This Works

A test fake is only as trustworthy as the source its author consulted. When the client and its
real server live in the same repository — the broker's `dispatch.Client` and Dispatch's own
`internal/dispatch/api` handlers both sit under `packages/envoy` — writing a fake by reading the
client's own request/response types produces a fake that is *definitionally* consistent with the
client, whatever the client gets wrong. Every verification layer here (task review, a 14-case
e2e suite, a fresh live-binary pass) tested the client against a fake with this property, so all
three agreed with the bug instead of catching it. Only a test that skips the fake entirely — the
real client against the real server's own handler, even in-process — can observe that the two
sides' assumptions about the wire disagree, because at that point there is no third party's
belief standing between them.

## Prevention

- When a client and its server are both in this repository, write at least one permanent test
  that drives the client against the server's own real handler (an in-process `httptest.Server`
  wired to the server's own `Register`/`NewDeps`, or an equivalent public test harness) for every
  route the client calls. Do this once, as a contract test, rather than relying on task-scoped
  review or an end-to-end suite whose own fake was written the same way the client was.
- Do not trust a fake server's fidelity because it is exercised by a large or "live-feeling"
  suite (an e2e scenario, a live-binary pass). Ask instead what the fake's author read to build
  it — the real server's serialization code, or the client's own types. If it was the client's
  types, the fake cannot fail the client on this class of bug.

## Related Issues

- `docs/solutions/testing/integration-seam-needs-producer-exact-value.md` — the same principle
  (only a probe against the real producer's exact value catches an assumed-contract mismatch),
  for an untyped env-var channel between two packages rather than a JSON wire shape.
- `docs/solutions/testing/fixtures-derive-what-production-derives.md` — a test helper's hardcoded
  construction value diverging from what production derives; here the divergence is the fake
  server's *response body shape* rather than one constructor argument.
- `docs/solutions/testing/a-test-over-a-contract-derives-the-enumeration-and-hand-writes-only-what-the-contract-cannot-prove-about-itself.md` —
  deriving what a test can prove from the real contract instead of hand-writing an assumption
  about it.
