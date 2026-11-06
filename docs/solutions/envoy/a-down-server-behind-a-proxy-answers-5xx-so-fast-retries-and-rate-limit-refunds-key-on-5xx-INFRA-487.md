---
title: "A down server behind a proxy answers 5xx, so fast retries and rate-limit refunds key on 5xx"
category: envoy
tags:
  - retry
  - backoff
  - reverse-proxy
  - rate-limit
  - outage
date: 2026-10-27
status: active
module: packages/envoy
related_issues:
  - "INFRA-487"
  - "sjawhar/legion#2269"
---

# A down server behind a proxy answers 5xx, so fast retries and rate-limit refunds key on 5xx

- A client that retries quickly "while the server is down" treats a transport error **and any
  5xx** as down. Behind a reverse proxy (Tailscale serve is an `httputil.ReverseProxy`), a dead
  backend answers `502`, and a backend that lost its database answers `500`. Retrying fast only on
  a transport error puts the client on its slow refusal schedule for exactly the outage it should
  ride out. Keep 4xx (429 included) on the slow schedule.
- Test the classification with a real `httputil.ReverseProxy` in front of a closed port, not only a
  fake server that drops the connection.
- A server rate limit that a retrying client spends must not run dry during the server's own
  outage. Give the request's tokens back when the server answers 5xx **after** the credential has
  verified, since the server decided nothing about the caller. Never give them back:
  - before the credential verified, because the bucket spent there is what limits the expensive
    verification (here, the issuer's key fetches), whatever the failure is answered;
  - after any 4xx, because the limits exist to slow guessing.
- Pair each refund with a test that a refused credential still reaches `429`.

## Evidence

- The Legion daemon's token login retried at 2 s→10 s only on a transport error
  (`packages/daemon/internal/agentsecrets/client.go`); the tester showed the production front door
  answering `502` and the broker `500 INTERNAL` with its Postgres stopped, both landing on the
  30 s→5 min schedule (commit 18abf099 widened the class to 5xx; tester red tests in 8502875f).
- The broker's `POST /v1/launcher-credentials/token` spent its per-account bucket (burst 5) before
  the database write that failed, so the daemon was refused `429` after five retries and logged in
  23.8 s after Postgres returned; with the refund (commit 6df6138d,
  `internal/broker/api/handlers_launcher.go`, `ratelimit.Keyed.TakeAt`) a 240 s outage gave 26×500,
  no 429, and a login 3.8 s after. Commit fa1312aa removed the refund after a failed verification;
  `TestARefusedTokenLoginStillSpendsItsAccountBucket` shows five `403`s then `429`.
