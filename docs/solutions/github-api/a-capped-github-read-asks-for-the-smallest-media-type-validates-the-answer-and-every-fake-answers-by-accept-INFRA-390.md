---
title: "A capped GitHub read asks for the smallest media type, validates what comes back, and every fake answers by Accept"
category: github-api
tags:
  - github-rest
  - response-cap
  - media-type
  - test-fakes
  - dispatch
date: 2026-10-25
status: active
module: packages/envoy/internal/dispatch/githubapp
related_issues:
  - "INFRA-390"
  - "sjawhar/legion#2202"
---

# A capped GitHub read asks for the smallest media type, validates what comes back, and every fake answers by Accept

- **Ask GitHub for the smallest answer that holds the value.** Dispatch's `githubapp.Client`
  refuses any body over 1 MiB (`responseLimit`). A read that needs one value from a large object
  sends a media type, `per_page`, or a narrower endpoint. For a branch head, send
  `Accept: application/vnd.github.sha` on `GET /repos/{o}/{r}/commits/{ref}`. GitHub then answers
  40 bytes of hex with no newline, where the JSON answer carries up to 300 files with their patches.
- **Validate what comes back.** A proxy, a fake, or GitHub itself can ignore the media type and
  answer JSON. Treat the body as a value only when it matches its exact shape (`^[0-9a-f]{40}$`).
  Make every other body an error that names the byte count, never the body text. This matters
  most when a later read turns "not found" into "empty". `Dir` answers GitHub's 404 as an empty
  directory, so a commit id that was really JSON would import an empty architecture model over
  the real one. An error from `Ref` keeps the previous model.
- **Read the error statuses again under the new media type, against real GitHub.** A missing
  branch answers **422**, not 404, both under the SHA media type and under JSON. Write fakes to
  answer GitHub's real status, not the one the client's doc comment assumes.
- **Every fake of the route answers by `Accept`.** Find them all: the Go fakes in every package
  whose tests reach the route, and the browser suite's `packages/dispatch/e2e/fake-github.ts`.
  Each answers the bare value for the media type and JSON otherwise. Change fixtures that would
  fail the new validation (`"sha-one"`, `"c0ffee"`, `commit-%d`) to named valid values. Keep every
  equality and every change between commits that the test had.
- **A size test measures the fake's own uncapped answer before it calls the client.** Otherwise a
  fake that never grew past the cap passes for the wrong reason.

## Evidence

- On sjawhar/legion `scratch/infra-390-large-head` (320 files of about 7 KB), the JSON answer was
  1,239,400 bytes. Before the fix, `Ref` failed with `GitHub response body exceeds the
  1048576-byte limit`. Under the SHA media type, the tester's proxy logged `-> 200 40 bytes`.
  This is the second failure from the same cap. `delivery/reconcile_volume_test.go` records the
  first, on pages of workflow runs.
- The tester dropped `Accept` in a proxy between Dispatch and GitHub. The sync then failed with
  `commit for sjawhar/legion@main: GitHub answered no commit SHA (31100 bytes)` and kept
  `last_commit` and all 36 components.
- A missing branch answered 422 under the SHA media type (the tester's run, and the planner's
  probe under JSON). Every Go fake in the pull request still answers 404 for a missing branch, so
  no test covers 422 → `ErrNoBranch` (`client.go`, `Ref`). The status mapping was kept unchanged
  and the live run covers it.
- Six fakes needed the `Accept` branch: `client_test.go`, three handlers in
  `architecture/importer_test.go`, `api/architecture_source_test.go` (with
  `issue_components_test.go` using it), and `e2e/fake-github.ts`.
  `TestRefReadsTheHeadOfALargeCommit` asserts `recorder.Body.Len() > responseLimit` on the fake's
  JSON before it calls `Ref`.
