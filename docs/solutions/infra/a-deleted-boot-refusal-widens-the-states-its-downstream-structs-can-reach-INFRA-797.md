---
title: "A deleted boot refusal widens the states its downstream structs can reach"
category: infra
tags:
  - boot
  - configuration
  - authorization
  - dispatch
date: 2026-10-30
status: active
module: packages/envoy
related_issues:
  - "INFRA-797"
  - "sjawhar/legion#2553"
---

# A deleted boot refusal widens the states its downstream structs can reach

Extends docs/solutions/infra/when-a-boot-dependency-stops-refusing-the-boot-every-reader-of-the-old-exit-reads-the-new-health-field-pr2390.md.

That document censuses the readers *outside* the process — scripts, deployers, probes — that
read the exit the refusal used to give. Census the states *inside* it too.

A boot refusal is a constraint on every struct the process builds afterwards. Deleting it adds
configurations to the input domain of code that never had to handle them, and that code does not
change, so nothing in the diff shows the widening. Before you delete one, list the combinations
it made impossible and decide each in writing. Each one is a question about run-time behaviour,
not about whether the process starts: for each, name what the new configuration reaches, and say
whether that is what you want.

## Evidence

INFRA-797 removed `requireCookieSignIn` (`packages/envoy/cmd/dispatch/main.go`, the cookie arm of
`resolveBootConfig`'s identity switch) so `envoy-dispatch` could boot with no `DISPATCH_SIGNIN_*`
setting. Every line of `requestIdentityFor` and `CookieIdentity.Login` was correct for the
configurations that could exist before, and none of them changed. The deletion made one new
combination constructible — cookie identity with no issuer — under which `CookieIdentity` is
built with `Membership` nil and honours every session cookie the deployment's persistent
`DISPATCH_SIGNING_KEY` still verifies, for thirty days, confirming nothing. The review round that
found it put the test plainly: before this change that server did not boot.
