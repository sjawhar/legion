---
title: "A branch exists only by its exact ref read: commits/{ref} resolves any commit-ish, and a fake never echoes the name back"
category: github-api
tags:
  - github-rest
  - branch
  - commit-ish
  - test-fakes
  - dispatch
date: 2026-10-26
status: active
module: packages/envoy/internal/dispatch/githubapp
related_issues:
  - "INFRA-391"
  - "sjawhar/legion#2209"
---

# A branch exists only by its exact ref read: commits/{ref} resolves any commit-ish, and a fake never echoes the name back

Extends docs/solutions/github-api/a-capped-github-read-asks-for-the-smallest-media-type-validates-the-answer-and-every-fake-answers-by-accept-INFRA-390.md.

- **Resolving a head is not proving a branch exists.** `GET /repos/{o}/{r}/commits/{ref}`
  answers 200 for a full SHA, `main~1`, `main^`, `HEAD` and `refs/heads/main`. Use it to resolve
  a stored branch's head (Dispatch's sync does, with `githubapp.Client.Ref`). Never use it to check
  a value a person typed as a branch.
- **To prove a name is a branch, read its exact ref.** Call
  `GET /repos/{o}/{r}/git/ref/heads/{url.PathEscape(name)}` (`githubapp.Client.Branch`). Escape the
  whole name: GitHub resolves a raw `foo/../x` to the branch `x`, and answers 404 for the escaped
  `foo%2F..%2Fx`. Accept a 200 only when its `ref` is exactly `refs/heads/<name>` and its SHA is
  40 hex. Treat 404, 409 (an empty repository) and 422 as "no such branch". Do not refuse a value
  for looking like a SHA: a branch can have that name, and the exact read decides.
- **A fake of an existence check answers 200 only for the names its fixture seeds.** A fake that
  answers any ref hides a check built on the wrong read. The Go fake's `commits/{branch}` answered
  every value, so a save check built on `Ref` passed every SHA and `main~1` in tests.
- **A fake never copies the requested value into the field the client compares.** If it echoes
  `ref: refs/heads/<requested name>`, the client's `ref == refs/heads/<name>` check compares the
  value with itself and always passes.

## Evidence

- Review round 2 of sjawhar/legion#2209 (thread PRRT_kwDORFy7ds6wHH8t, at fa2dacfbb399) probed
  real GitHub: every commit-ish answered 200 from `commits/…` and 404 from `git/ref/heads/…`.
  The fix is de00bf0aac. `TestArchitectureSourcePutRefusesACommitThatIsNoBranch` failed on the
  `Ref`-based handler.
- Proof on real GitHub: https://github.com/sjawhar/legion/pull/2209#issuecomment-6406509456. At
  the head, the SHA, `~1`, `^`, `HEAD`, `refs/heads/…` and the prefix `infra-391` each got `409`
  with no row. The previous head stored the first five with `200`.
- Left at the approved head: `Ref`'s doc comment (`githubapp/client.go`) still says the call
  "also proves the branch exists". The browser suite's `packages/dispatch/e2e/fake-github.ts`
  answers the exact read for any name on a seeded repository and echoes it as `ref`, so no browser
  spec can observe a refused branch. The refusal is proven only by the Go tests and the
  real-GitHub rig.
