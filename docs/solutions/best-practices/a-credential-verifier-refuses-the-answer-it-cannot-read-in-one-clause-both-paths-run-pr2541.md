---
title: "A credential verifier refuses the answer it cannot read, in one clause both its paths run, and takes the host's trust policy as a required option"
category: best-practices
tags:
  - authentication
  - fail-closed
  - shared-package
  - introspection
date: 2026-10-31
status: active
module: packages/envoy/internal/brokertoken
related_issues:
  - "sjawhar/legion#2541"
---

# A credential verifier refuses the answer it cannot read, in one clause both its paths run, and takes the host's trust policy as a required option

A package that decides whether a credential is good has two ways of being wrong that no test of
the happy path reaches.

- **Refuse an issuer's answer you cannot read, rather than reading it as "names nothing".** An
  answer missing the session, missing the person, issued to another client, carrying a scope word
  the package does not know, or carrying a known scope whose companion field is empty, is a token
  the package cannot judge. Every one of those read as *unscoped* in an earlier draft, which is
  the widest authority there is. `Verifier.live`
  (`packages/envoy/internal/brokertoken/brokertoken.go`) now refuses all five in one condition,
  with the reason on the clause: the scope says what `Project` means, so a word the package does
  not know is a token it cannot read.
- **Put that clause where every path runs it.** A credential checked once per request and
  re-checked on a websocket's heartbeat has two entry points; give them one private function
  (`Verify` and `Live` both call `live`) so a rule added to one is a rule for both. Three review
  rounds each found one more unreadable answer, and each fix was one clause because of this.
- **Take the host's trust policy as a required constructor option, never by importing the
  host.** The verifier needs the source address its rate bucket is keyed on, which depends on
  which proxy the listener trusts — a decision the host owns. `Options.Address` is required and
  has no default (`New` refuses a verifier built without one), where importing the host's
  `ForwardedAddress` would both contradict the package's own boundary and fix one listener's
  proxy policy for every other.

## Evidence

sjawhar/legion#2541 (INFRA-264). Round 1 found a machine login's own access token admitted, with
the request body then choosing the session it wrote as; round 2 found an unknown `project_scope`
read as unscoped; round 3 found `named` with an empty `project` read the same way. The fixes are
`02b80896`, `a1f34c47` and `98f089a9`, each one clause of `live`. Round 1 also found the
package importing `internal/dispatch/auth` for the forwarded address against its own package
doc; `Options.Address` replaced it in `02b80896`.
