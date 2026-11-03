---
title: "When a boot dependency stops refusing the boot, every reader of the old exit reads the new health field"
category: infra
tags:
  - healthz
  - boot
  - deploy
  - rollback
  - census
date: 2026-10-28
status: active
module: packages/envoy/cmd/dispatch
---

# When a boot dependency stops refusing the boot, every reader of the old exit reads the new health field

A process that used to exit at boot (or answer 503) when a dependency was down, and now starts and
serves, has moved the signal "this dependency is broken" from its exit status into a health field.
The old exit was a contract with readers that never named it.

- **Census the readers of the exit, not only of the field.** Search for every script, test and
  deployer that starts the binary and waits for it to die, or that reads a health `200` as "the
  dependency works": `wait_for_health` loops, proof scripts that expect a refusal, harnesses that
  start two copies.
- **A deployer's health gate reads the new field**, so an image whose dependency fails is still
  rolled back as it was when it exited at boot. Decide what an unset field (the dependency turned
  off) means, and test both.
- **A proof script that expected the refusal now drives the new behaviour**: start the binary in
  the background, wait for the health field, and assert what a caller of the missing dependency is
  answered.
- **Readiness and liveness are read separately**: a readiness probe that read the dependency
  through the old exit no longer does; say in the component's comments which dependency each
  probe reads.

## Evidence

- Envoy-dispatch stopped exiting when NATS or an OIDC issuer did not answer at boot. The review's
  contract change census found three readers the change had broken:
  - `packages/envoy/deploy/scripts/autodeploy.sh` `wait_for_health` read the status alone, so an
    image whose NATS credential was refused would have deployed and been kept. It now requires
    `nats` not `false` (`null`, NATS off, passes), with `nats:false` and `nats:null` cases in
    `autodeploy_test.sh` (718a8fa4).
  - `scripts/e2e/verifiers-staging-token.sh` ran Dispatch in the foreground against an absent
    issuer and waited for it to exit, which would now block forever. It now waits for `/healthz`
    `service_tokens:false` and asserts `503 OIDC_ISSUER_UNAVAILABLE` (626daf63, 961c6f1d).
  - `packages/envoy/infra/gcp/dispatch_kube.go` and `packages/envoy/infra/README.md` said the app's
    readiness read NATS (718a8fa4, 62a8f2d1, 75c172e2).
- None of the three failed a test of the change itself; only the census found them.
