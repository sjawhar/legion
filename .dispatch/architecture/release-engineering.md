---
title: Release engineering
parent: legion
paths: [.github/workflows, .github/scripts]
---
The cross-package automation no single package owns: the pull-request and main gate, the PR-title check, per-package semantic-release for the TypeScript packages and the Envoy listener, the path-scoped Envoy and contracts build, and the worker-image build that publishes `ghcr.io/sjawhar/legion-worker` by digest for the Kubernetes runtime to pin. `.github/scripts` holds the version-bump and release-push scripts those workflows call, with their own shell test. Every workflow's Postgres service is the pgvector-on-PostgreSQL-18 image Envoy's `internal/testpg` pins, by the same digest, and `envoy-go` prints the server's version and pgvector's before its tests; the one exception, the `dispatch-go-postgres-16` job, runs Dispatch's Go suite on production's Postgres 16 until dispatch://INFRA-572, after which dispatch://INFRA-573 removes it.
