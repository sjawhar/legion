---
title: "Prove which credential a caller presents without minting or printing one"
category: testing
tags:
  - credentials
  - identity
  - operator-docs
  - negative-control
  - dispatch
date: 2026-10-29
status: active
module: docs
related_issues:
  - "INFRA-228"
  - "sjawhar/legion#2503"
---

# Prove which credential a caller presents without minting or printing one

To prove a claim about which identity a caller presents (for operator documentation, or a change
to how credentials flow):

1. **Use the system's own identity command as the oracle.** For Dispatch that is `dispatch whoami`,
   which reads `DISPATCH_TOKEN_FILE` and says whose token it is (`runs under the shared token: no
   owner` for a shared agent token).
2. **Control it with a one-byte-flipped copy of the credential file** (`DISPATCH_TOKEN_FILE`
   pointed at the copy). A refusal shows the answer came from that file. Print only the count of
   differing bytes.
3. **Prove that two principals hold the same secret with a salted SHA-256** of each file, and
   print only the length and `equal` or `differs`. A role cannot read another role's private
   directory, so send the salt and your digest to the other role and let it run the comparison in
   its own container. Control the comparison with the same salted digest of a one-byte file.
4. **Probe production only with calls the system refuses**, and add a control that shows the
   endpoint serves the feature, so the refusal names the configuration and not a dead route. An
   endpoint that mints on success gets no successful call in production.
5. **Get the positive case (a minted token and what it carries) from the real binary built from
   your branch**, on a throwaway store, and add a refused case (an identity no entry lists).
6. **Before you write "an unset setting disables X", read the registration code for every entry
   point of X.** A setting usually gates one branch, and a live refusal tells you only which
   branch refused.

## Evidence

sjawhar/legion#2503, `.legion/INFRA-228/implement.json` proof rows 1-4:

- `dispatch whoami` in a role answered `no owner`. The flipped-byte copy gave `invalid bearer
  token`, exit 1.
- The architect ran the salted digest in its own container and printed `equal`. The tester
  repeated it with its own salt.
- The production broker refused the Kubernetes subject token type as `not supported`, and refused
  a fake access token as `Token is not valid or has expired`. The second refusal is the control:
  the access-token type stays registered.
- `packages/envoy/scripts/dev-broker.sh`, built from the branch on an embedded Postgres, issued a
  DPoP-bound token. Its introspection carried exactly the four documented claims, with `jkt` equal
  to the client's key thumbprint. A service account that no entry lists was refused by name.

The approved plan said "a deployment that configures no issuer exchanges nothing". That is false:
`ExchangeConfig.tokenTypes` always registers the access-token type, and `Set.Machine` needs no
entry (`packages/envoy/internal/broker/oauth/exchange.go`,
`packages/envoy/internal/broker/exchange/exchange.go`). The docs were written to the code instead.
