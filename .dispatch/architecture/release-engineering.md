---
title: Release engineering
parent: legion
paths: [.github/workflows, .github/scripts]
---
The cross-package automation no single package owns: the router `ci.yaml`, the pull-request and main gate that selects its jobs by the paths a change touched, with the docs build and the pull-request worker-image build among them and `ci-ok` condensing their results into one check; the PR-title check (`pr-title.yaml`); the release workflows (`release.yaml` for the `legion` binary and the npm plugins, `release-envoy-listener.yaml` for the Envoy listener, `release-envoy-infra.yaml` for the `envoy-infra` Pulumi plugin); and the worker-image build (`worker-image.yaml`) that publishes `ghcr.io/sjawhar/legion-worker` by digest for the Kubernetes runtime to pin. `.github/scripts` holds the checks those workflows run and the version-bump and release-push scripts they call, with their shell tests.
