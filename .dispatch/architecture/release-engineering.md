---
title: Release engineering
parent: legion
paths: [.github/workflows, .github/scripts]
---
The cross-package automation no single package owns: the pull-request and main gate, the PR-title check, per-package semantic-release for the TypeScript packages and the Envoy listener, the path-scoped Envoy and contracts build, and the worker-image build that publishes `ghcr.io/sjawhar/legion-worker` by digest for the Kubernetes runtime to pin. `.github/scripts` holds the version-bump and release-push scripts those workflows call, with their own shell test.
