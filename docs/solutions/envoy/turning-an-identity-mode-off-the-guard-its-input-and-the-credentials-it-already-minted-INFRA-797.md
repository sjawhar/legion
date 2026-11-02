---
title: "Turning an identity mode off: the guard, its input, and the credentials it already minted"
category: envoy
tags:
  - authorization
  - identity
  - dispatch
  - sign-in
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "INFRA-797"
  - "sjawhar/legion#2553"
---

# Turning an identity mode off: the guard, its input, and the credentials it already minted

Making an identity provider optional is three decisions, not one. Take all three before the
configuration that turns it off can exist.

**An absent collaborator is a distinct type, never a struct field a later branch reads as
permission.** `identity.CookieIdentity` gives `nil` two opposite meanings: `Sessions == nil`
refuses (`internal/dispatch/identity/identity.go:48`), while `Membership == nil` *skips* the
membership confirmation (`:58`). The fail-open one is the security-relevant one, and
`requestIdentityFor` builds that struct field by field, so the dangerous value is a default
nobody wrote. An identity that must name nobody is its own type with no fields and no
optionality — `identity.NoBrowserIdentity` (`identity.go:89`) — returned before any
`CookieIdentity` is constructed (`cmd/dispatch/signin.go:190-191`). Then a later change to
`Login` cannot reopen it.

**A guard that asks what the deployment configured reads the configuration, not a value derived
from it.** `signInFlow(boot)` and `boot.SignInIssuer` answer the same question in `main`, but the
derived flow has more constructors than production has: `settings_test.go`'s `appContextFor`
passes `built.SignIn`, which a case leaves nil to mean "cookie identity with no membership
check". A guard keyed on `signIn == nil` read that as "this deployment configured no sign-in"
and refused six cases the deployment would never produce. The guard reads
`boot.SignInIssuer == ""` (`cmd/dispatch/signin.go:190`), and says why in its own comment.

**Removing the provider does not remove the credentials it minted. Census each class, and check
each revocation route still has an actor.** Decide, per class, refuse-now, let-expire, or
revoke-before-cutover:

- session cookies are refused now, because the deployment's own `DISPATCH_SIGNING_KEY` outlives
  the setting and goes on verifying them for their thirty days;
- a personal agent token minted while sign-in was on keeps acting for its person, and
  `revokeAgentToken` needs a person (`internal/dispatch/api/agent_tokens.go:112`), so with
  sign-in off nobody can revoke it through Dispatch. That one is an operator precondition
  (`packages/envoy/infra/README.md`: revoke those tokens before a stack drops its sign-in
  inputs), not a code fix.

A credential whose revocation route needs the mode you removed is unrevokable, not merely
inconvenient. Say so where the operator reads, before the mode can be turned off.

## Evidence

INFRA-797 made `envoy-dispatch` boot with no `DISPATCH_SIGNIN_*` setting by deleting
`requireCookieSignIn`. The review found that a cookie-identity server with no issuer then built
`CookieIdentity{SigningKey, Sessions}` with `Membership` nil and honoured every `dsession`
cookie an earlier sign-in had minted, for its thirty days, with nothing re-confirming the person:
a stack that dropped its sign-in inputs kept every browser session live, including one belonging
to somebody since removed from the domain, who could still mint personal tokens that do not
expire.

Driven on the real binary, against a database carrying the `people` and `user_sessions` rows a
sign-in leaves behind: with the guard, `GET /api/v1/whoami`, `GET /api/v1/inbox` and
`POST /api/v1/me/agent-tokens` all answer `401 NO_IDENTITY`; with the guard disabled, the same
three answer `200 {"kind":"user",…}`, `200` and `201` with a usable token. Dev sign-in is
untouched — it mints its own cookie and keeps it — which is why the guard carries
`&& !boot.DevSignIn`.
