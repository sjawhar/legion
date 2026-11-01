---
title: "A stand-in daemon proof proxies the credential routes the pane's tools call, and an App installation token reads its identity through GraphQL viewer"
category: testing
tags:
  - stand-in
  - oh-my-pi
  - gh
  - github-app
  - installation-token
  - graphql-viewer
  - legion-grant
  - negative-control
date: 2026-10-10
status: active
module: packages/pi-legion
related_issues:
  - "LEGION-663"
  - "sjawhar/legion#1874"
---
# A stand-in daemon proof proxies the credential routes the pane's tools call, and an App installation token reads its identity through GraphQL viewer

Extends docs/solutions/testing/driving-a-real-omp-worker-against-a-stand-in-daemon.md.

- A scratch `omp --mode rpc` session boots the branch's plugin against a bun stand-in for
  `claims/register`, `claims/ready` and the Envoy listener (`ENVOY_NATS_URL` unset: pi-envoy
  warns and skips inbound delivery; the role claim is two listener calls) with the pod's real
  profile, search provider and Dispatch. When a measured row runs a credentialed command — the
  pane's `gh` — the stand-in must also serve or proxy the credential route that command's shim
  calls (`POST /legion/v1/gh-token` under today's grant shim, to the pod's own daemon:
  `LEGION_DAEMON_URL` of your pane; the grant your own last bash call wrote is live for 60 s).
  A stand-in without it makes the row fail on the stand-in's gap (`no stand-in route
  /legion/v1/gh-token`), which hides the surface's own fact. Write the row's failure into the
  proof as what it is; a failure you explain as the stand-in's is one the tester will re-run with
  the route served, and find the real one.
- A Legion role's `gh` runs on a GitHub App installation token. GitHub answers REST `GET /user`
  to one with `403 Resource not accessible by integration`, whatever the token's permissions.
  An installation token reads its own identity with
  `gh api graphql -f query={viewer{login}} --jq .data.viewer.login` → `legion-<role>[bot]`, the
  read `packages/daemon/internal/api/real_github_test.go` and `scripts/e2e/stage4b-sandbox-tree.sh`
  make. A spec or plan that says `gh api user` is naming an endpoint the credential cannot call;
  tell the architect, and use the viewer read.
- A proof's negative control for a credentialed row is a wrapper first on `PATH` that runs the
  real binary with the credential removed (`/usr/local/bin/gh` under an empty `GH_CONFIG_DIR`,
  `GH_TOKEN`/`GITHUB_TOKEN`/`LEGION_GRANT_FILE` unset): the row alone fails, naming gh's own
  sentence, and the session still reaches ready.

## Evidence

LEGION-663 round 1: the implementer's stand-in served the claim routes and the listener alone;
the `github` row read `gh api user failed: legion gh: Unable to redeem LEGION_GRANT: daemon
returned 404: {"error":"no stand-in route /legion/v1/gh-token"}` and the proof recorded it as the
stand-in's gap. The tester proxied `/legion/v1/gh-token` to the pod's daemon, fed the reviewer
App's real token, and the row read `gh api user failed: gh: Resource not accessible by integration
(HTTP 403)` — the real fact the stand-in had hidden — and failed the round with a red test. Round 2
moved `checkGitHub` to the viewer query; the implementer's proof with the proxy read `gh viewer
login: legion-implementer[bot]`, the tester's `legion-reviewer[bot]`, and the credential-less
wrapper read `gh viewer login failed: gh: To use GitHub CLI in automation, set the GH_TOKEN
environment variable.` with the other five rows passing.
