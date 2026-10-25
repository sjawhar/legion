---
title: "A contract census names refusals too and lists every hit, tests and the PR's own files included; a finite field is an enum"
category: legion
tags:
  - contract-change-census
  - daemon-api
  - zod
  - pr-body
  - refusal
date: 2026-10-25
status: active
module: packages/contracts
related_issues:
  - "INFRA-229"
  - "sjawhar/legion#2180"
---

# A contract census names refusals too and lists every hit, tests and the PR's own files included; a finite field is an enum

Extends docs/solutions/legion/a-strict-schemas-new-required-field-is-censused-across-every-parser-rigs-and-stand-ins-included-LEGION-578.md.

- A new refusal on a route (a new 409 code) is a contract change at a process boundary, as much as
  a new required field is. Give each change its own search in the `## Contract change census`, and
  write the census from the pull request's first push: the reviewer requires it
  (`packages/daemon/internal/prompts/roles/core/reviewer.md`) and re-runs its searches.
- List every hit with a disposition, `unaffected` included: tests that pin the route or the
  operation's name, the client's route table test, and the pull request's own new files (its live
  proof). The reviewer counts any hit missing from the list as a finding, whatever its
  disposition.
- A daemon API field over a finite Go set (`record.PullRequestState`) is a `z.enum` of exactly
  those values in `packages/contracts/src/legion-api.ts`, never `nonEmptyString`: every other
  finite domain there is an enum. Rebuild the claude-envoy bundle in the same push
  (`bun run --cwd packages/claude-envoy build`, then `check-dist`), and prove the enum by parsing
  the golden with an out-of-set value.

## Evidence

- INFRA-229's first review found no census for its two changes: `POST /legion/v1/signoff` now
  answers 409 `SIGNOFF_CHILD_IN_WORKFLOW`, and `pullRequest.state` became required under contract
  36. Once the census was written, the reviewer's re-run of its first search found three hits
  missing: `prompts_test.go:31`, `daemon-client.test.ts:234` and the PR's own
  `merged_root_waits_live_test.go`. Each was `unaffected` and was left out for that reason.
- `state` shipped as `nonEmptyString`, with a doc comment naming its three values. As
  `z.enum(["open", "merged", "closed"])`, the golden parses and a `state: "draft"` copy is refused
  with `Invalid option: expected one of "open"|"merged"|"closed"`.
