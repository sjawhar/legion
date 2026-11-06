---
title: "A pod Postgres that serves the broker's suites needs a password for every role but the superuser, and internal/broker/helper runs in no pod at all"
category: testing
tags:
  - issue-pod
  - postgres
  - harness
  - gvisor
  - broker
date: 2026-10-31
status: active
module: packages/envoy/internal/broker
related_issues:
  - "sjawhar/legion#2541"
---

# A pod Postgres that serves the broker's suites needs a password for every role but the superuser, and internal/broker/helper runs in no pod at all

Extends docs/solutions/testing/a-pod-postgres-for-dispatch-tests-is-initialised-with-icu-collation-so-the-rank-order-test-runs-pr2397.md.

That note gives the `initdb` line for Dispatch's suites. Two more facts hold once the same server
also runs the broker's.

- **`initdb`'s default `pg_hba.conf` trusts every host connection, and one broker test needs a
  role that does not get in without a credential.** `internal/broker/store`'s IAM test opens the
  store without its token minter and requires Postgres to refuse the connection; under `trust` it
  succeeds and the test fails by its own message, `the test proves nothing` — which reads like a
  branch defect and is the harness. Leave the superuser trusted and require a password of every
  other role, then reload:

  ```
  host    all             postgres        127.0.0.1/32            trust
  host    all             all             127.0.0.1/32            scram-sha-256
  ```

  Put it in the harness script, guarded on the first of those two lines, so the next role that
  re-runs the script inherits it.
- **`internal/broker/helper` runs in no issue pod**, whatever the database does: it identifies its
  peer with `SO_PEERPIDFD`, which gVisor answers `protocol not available`, so every contract test
  of the helper, of `cmd/agent-secrets` and of `cmd/agent-secrets-helper` fails
  `PEER_UNIDENTIFIED`. Name the package in the handoff's not-run list and let CI's `envoy-go`
  cover it; it belongs beside the Docker and collation cases in
  docs/solutions/testing/the-dispatch-servers-go-suites-in-an-issue-pod-run-api-and-docs-alone-with-a-timeout-and-read-the-c-collation-docker-and-bun-failures-as-the-environment-LEGION-752.md,
  which does not name it.
- **The broker's own dev stack needs no Docker and no cluster**: `scripts/dev-broker.sh` takes
  `DEV_BROKER_POSTGRES_URL` and creates its own database on a server you already run, starts the
  repository's `cmd/dev-oidc-issuer` in a cluster's place, and configures the token exchange for
  the run. Paired with `packages/dispatch/e2e/run-server.sh` and
  `DISPATCH_E2E_AGENT_SECRETS_URL`, that is a real broker and a real Dispatch on one pod's
  loopback.

## Evidence

sjawhar/legion#2541 (INFRA-264), round 5. The pod's harness was rebuilt from nothing after the
workspace was recreated; with `initdb`'s defaults,
`TestOpenSignsEveryNewConnectionInWithAFreshlyMintedToken` failed `the test proves nothing`, and
the whole `internal/broker/store` package was green at once after the `pg_hba` change and a
`pg_ctl reload`. The same run put the broker and Dispatch on that server through
`DEV_BROKER_POSTGRES_URL` and `DISPATCH_E2E_AGENT_SECRETS_URL`, and drove the token exchange and
every scope refusal over it. `internal/broker/helper` failed `PEER_UNIDENTIFIED` in that pod on
every attempt.
