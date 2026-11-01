---
title: "A key variable a secret store fills accepts the stored form, and is parsed at load naming the variable"
category: infra
tags:
  - secrets
  - gke-secret-sync
  - secret-manager
  - pem
  - dispatch
  - boot
date: 2026-10-25
status: active
module: packages/envoy/internal/dispatch/auth
related_issues:
  - "INFRA-390"
  - "sjawhar/legion#2202"
---

# A key variable a secret store fills accepts the stored form, and is parsed at load naming the variable

- **A secret-backed variable gets the value exactly as the store holds it.** A variable named
  `*_B64` gets no base64 from GKE Secret Sync or a `secretKeyRef`. When the store holds the PEM a
  GitHub App download gives, the process receives that PEM. Accept the stored form. Do not ask the
  operator to re-encode a secret by hand.
- **Tell the two forms apart by a marker that cannot be confused.** A value containing
  `-----BEGIN` is a PEM. The standard base64 alphabet has no `-`, so no value that boots today as
  base64 is read as a PEM. Base64-decode any other non-empty value as before. An empty value still
  means "no key".
- **Parse the key where the variable is read.** Name the variable and the form in the error
  (`DISPATCH_APP_PEM_B64: plain PEM: not a PKCS1 or PKCS8 private key`) and never quote the value.
  Without this, a bad key fails later and deeper (`parse GitHub App private key: …` from
  `githubapp.New`), naming nothing an operator can act on. Before adding the load-time parse,
  check that every value it refuses was already refused later at boot. That makes the earlier
  refusal a change of message, not of contract.
- **Prove it with a form matrix on the real binary, at the head and at the base.**
  - Forms that should boot: PKCS#1, PKCS#8, CRLF line ends, a `Bag Attributes` preamble, trailing
    blank lines, `base64 -w0`, base64 wrapped at 76 columns, base64 of PKCS#8.
  - Forms that should be refused: EC, public, encrypted and base64-of-DER keys, literal `\n`
    escapes.
  - Grep every log for key-material lines.
  - At the base, every plain form should fail with `illegal base64 data at input byte N`.
- **What the store actually holds is a production observation.** A unit table cannot show it. The
  production check reads the boot line (`dispatch: loaded github app … source=env`) once the
  secret is wired.

## Evidence

- `auth.LoadAppFromEnv` (`packages/envoy/internal/dispatch/auth/app.go`) at sjawhar/legion#2202.
  Before the change, a plain PEM stopped boot at `cmd/dispatch/main.go` with
  `DISPATCH_APP_PEM_B64: illegal base64 data at input byte 0`.
- The tester's 16-form matrix on a binary it built itself showed every case above at `ec9df474`.
  At the base, base64 of DER passed load and failed later with `BuildAppContext: parse GitHub App
  private key: no PEM block found`, naming no variable.
- No `gcp:Dispatch` change was needed. The component already renders `DISPATCH_APP_PEM_B64` as a
  `secretKeyRef` (`infra/gcp/dispatch_kube.go`), so the deployment's pinned envoy-infra version
  stays as it is. In this issue, no one read the form the production Secret Manager entry holds.
  The root, dispatch://INFRA-191, observes it in its production check.
