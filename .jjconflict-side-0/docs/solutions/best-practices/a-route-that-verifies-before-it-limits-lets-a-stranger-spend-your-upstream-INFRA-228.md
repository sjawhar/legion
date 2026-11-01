---
title: "A route that verifies before it limits lets a stranger spend your upstream"
category: best-practices
tags:
  - rate-limiting
  - authentication
  - availability
  - oidc
date: 2026-10-25
status: active
module: packages/envoy/internal/broker/api
applies_when:
  - You replace a local credential check with one that reaches a network
  - A route authenticates a token against a published key set
  - You are adding a route to an existing authenticated class
related_issues:
  - "INFRA-228"
  - "sjawhar/legion#2187"
---

# A route that verifies before it limits lets a stranger spend your upstream

Authentication that costs a network call is work an unauthenticated caller can make you do. Spend
the bucket first.

- **Rate-limit before verification, not inside the handler.** A limiter the handler spends runs
  after authentication, so on a refused request it limits nothing. The bucket belongs in the
  authentication step, before the credential is read.
- **Cost out the unauthenticated path whenever the credential check changes.** Replacing a
  constant-time secret compare with a signature check against a published key set turns a free
  refusal into an upstream fetch. Ask, before the push: what does one request from a stranger now
  cost, and how many can they send?
- **A JWT verifier refetches the issuer's keys when it does not hold the token's key id.** That
  is correct behaviour for rotation and is the amplification: the issuer and audience a forged
  token must carry are configuration, not secrets, so they are not a barrier. Single-flight only
  merges requests that overlap in time.
- **A bucket per source address, not one global bucket**, or a flood denies the legitimate caller
  instead of itself. State in the comment and in the pull request body what an honest caller
  costs against it, what it sees when it trips, and what the bucket is keyed on behind a proxy.
- **One bucket per authenticated class, spent once per request.** A route that spends its own
  bucket after the class's is limited twice, and the second limit is the one nobody sized.
- **Look for the floor your codebase already has.** A key-set reader with a minimum refresh
  interval caps the fetches no matter how many addresses or replicas a flood uses; a per-address
  bucket caps them per address per process. Both are worth having, and the first is the one that
  bounds the cost against the issuer overall.
- **The rule is usually already written somewhere in the same file.** A service that learned this
  on one route states it in that route's comment; new routes do not inherit the comment.

## Evidence

INFRA-228 (sjawhar/legion#2187) moved nine broker routes off a shared bearer — a constant-time
compare needing no network — onto each caller's own projected Kubernetes ServiceAccount token,
and added an introspection route on the same check. All ten verified the token before any rate
limit; the introspection route's own limiter was spent inside its handler, after authentication,
so it limited nothing on that path. The reviewer's deep review drove 12 forged requests carrying
the configured issuer and audience, signed under an unknown key id, and counted 12 extra JWKS
fetches against the cluster issuer.

`packages/envoy/internal/broker/api/limits.go:13-21` already stated the rule for the broker's
token-login route — "A token login spends its address's bucket before its token is verified,
since a refused token signature makes the verifier fetch the issuer's keys again" — written for
exactly this reason and never connected to the new routes until review round 2.

The fix spends one per-address bucket for the whole class in `authenticate`, before
`VerifyAccount`, and the introspection handler spends nothing further. A residual remains and is
recorded: the buckets are per process, so a fleet of N replicas allows N times the rate, and a
flood spread over many addresses still reaches the verifier. This repository's own
`internal/oidc.Keys` carries a one-minute minimum refresh that would bound the fetches overall;
the exchange path uses it and the account-authentication path does not.
