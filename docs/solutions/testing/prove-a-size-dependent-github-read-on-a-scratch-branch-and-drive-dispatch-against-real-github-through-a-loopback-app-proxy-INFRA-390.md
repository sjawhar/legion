---
title: "Prove a size-dependent GitHub read on a scratch branch, and drive Dispatch against real GitHub through a loopback App proxy"
category: testing
tags:
  - live-proof
  - github-rest
  - git-data-api
  - dispatch
  - github-app
  - go-overlay
date: 2026-10-25
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "INFRA-390"
  - "sjawhar/legion#2202"
---

# Prove a size-dependent GitHub read on a scratch branch, and drive Dispatch against real GitHub through a loopback App proxy

- **Build the fixture with the git data API, with no clone.** Make one `POST git/trees` on main's
  tree with inline `content`, then one `POST git/commits` and one `POST git/refs` under a
  `scratch/<issue>-…` name. GitHub accepted a 2.3 MB tree body in one call. No workflow runs on a
  push to a branch other than main in sjawhar/legion.
- **Measure the answer itself, never file counts.** GitHub's commit JSON lists at most 300 files,
  and patch bytes decide its size. A 376-file main commit answered 1,025,463 bytes, under the cap.
  320 files of about 7 KB answered 1,239,400 bytes.
- **Make the live test prove its own precondition.** Gate it on environment variables
  (`DISPATCH_REAL_GITHUB_BRANCH`, `_REPO`, `_HOSTS_FILE`), since CI holds no App token. Read the
  answer once with no cap, and fail with "the fixture is too small" when it is not over the
  limit. Only then call the production client. Read the role App's installation token in-process
  from the gh `hosts.yml` that `$GH_CONFIG_DIR` names, with yaml.v3. Never log the token or the
  decode error, because a YAML error can quote the file.
- **The implementer owns the scratch branch.** The review App has `push: false` on sjawhar/legion.
  The implementer creates the branch, leaves it for the tester and the reviewer, and deletes it at
  retro. Record the creation and the deletion as comments on the pull request. A retro is not
  always the last one: when the issue comes back, point a new ref at the same commit
  (`POST git/refs` with its SHA; GitHub still holds the unreferenced commit) so the proofs read
  the same commit, and delete it again at the next retro.
- **Drive the real binary against real GitHub without the App's private key.** Point
  `DISPATCH_GITHUB_API_BASE` at a loopback proxy:
  - The proxy answers the App-JWT endpoints (installation lookup, token mint) itself.
  - It forwards every `/repos/{o}/{r}/…` read to `api.github.com` unchanged (path, query,
    `Accept`), under the role App's installation token.
  - Drive Dispatch over its own API (`PUT …/architecture-sources/{owner}/{repo}`,
    `POST …/architecture-sources/sync`, `GET …/architecture`).
  - The same proxy is the fault injector: drop or rewrite a header to show the client fails safe.

  State its limit in the proof: GitHub never checks a JWT that this binary signed.
- **A unit-level live test is half the proof.** Calling `Client.Ref` from a test proves the
  client, and the proxy rig proves the binary's import. Plan the binary path from the start.
- **An overlay negative control includes every caller of a changed signature.** This adds one
  rule to the `go test -overlay` technique in
  `docs/solutions/testing/a-pod-drives-the-rendered-configuration-itself-and-a-plans-ci-only-claim-is-checked-against-docs-solutions-INFRA-127.md`.
  When the change adds a parameter (`request(…, accept, …)`), map to its base content every
  non-test file of the package that the change touched (`client.go` and `deliveries.go`). If you
  do not, the base build fails, and that build failure is not the test's failure. Keep the test
  files at the head, using only symbols the base has, so the identical test runs against both.

## Evidence

- The implementer's proof at `ec9df474` was `TestRealGitHubRefReadsALargeHead` on
  `scratch/infra-390-large-head` (43aeb35d). It passed at the head with
  `Ref = 43aeb35d…`. It failed against the base overlay with `GitHub response body exceeds the
  1048576-byte limit`.
- The tester's proof used the proxy rig with the review App's token:
  - The sync answered 200 with `last_commit 43aeb35d…` and 36 components.
  - `main` imported at the same SHA that `gh api branches/main` shows.
  - A missing branch got GitHub's 422, and the sync answered 409 `SOURCE_ACCESS`.
  - With `Accept` dropped, the sync failed safe and kept the model.
  - The base binary recorded the 1 MiB error with `last_commit` null.
- The branch was created at 2026-10-25T15:48:34Z and deleted at 2026-10-25T18:07:09Z
  (sjawhar/legion#2202 comments 6400547094 and 6401241797). After the architect sent the issue
  back, a ref was pointed at the same commit 43aeb35d at 18:15:18Z for round 2's proofs
  (comment 6401406913).
