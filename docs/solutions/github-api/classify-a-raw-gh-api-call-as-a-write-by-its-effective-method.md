---
title: "Classify a raw `gh api` call as a write by its effective method: an explicit -X/--method wins, else POST when a body flag is present, else GET"
category: github-api
tags:
  - gh
  - gh-api
  - argv
  - classifier
  - legion-gh
  - refusal
  - http-method
date: 2026-09-13
status: active
module: packages/daemon/src/cli
problem_type: correctness
related_issues:
  - "LEGION-78"
  - "sjawhar/legion#1015"
applies_when:
  - A wrapper around `gh` must decide from argv alone whether a raw `gh api` call reads or writes
  - A guard keyed on `gh <subcommand> <verb>` needs to cover the raw REST route to the same endpoint
  - You are writing a positional-token predicate over an argv you do not fully parse
---

# Classify a raw `gh api` call as a write by its effective method

## The rule gh applies

`gh api` sends `GET` unless told otherwise, and "told otherwise" has two forms (`gh api --help`,
verified on this box; the reviewer traced it to `pkg/cmd/api/api.go`):

1. An explicit method: `-X <v>`, `--method <v>`, or the attached forms `-X<v>` and `--method=<v>`.
   The value is case-insensitive (`--method=patch` is a PATCH).
2. Otherwise, `POST` when any body flag is present: `-f`/`--raw-field`, `-F`/`--field`, or
   `--input` — exact, `=`-attached (`--field=k=v`, `--input=file`), or short-attached
   (`-fk=v`). `--input file.json` with no `--method` is a POST too; gh's own example creates a
   ruleset that way.
3. Otherwise `GET`.

The one consequence people miss: `--method GET repos/o/r/issues -f state=open` is a **GET with query
parameters**. A classifier that treats "any body flag" as a write refuses a read.

`ghApiMethod` in `packages/daemon/src/cli/index.ts` is that rule in twenty lines: walk argv, remember
the last explicit method, remember whether any body flag appeared, return
`explicit ?? (body ? "POST" : "GET")`; the caller uppercases before comparing.

## The endpoint side: match the path token, not the flag values

`isGitHubIssueWriteInvocation` combines the method with the same positional filter
`isPrMergeInvocation` uses — drop every token that starts with `-`, then test what is left:

- `api` is a positional, some positional matches `/\/issues(?:[/?]|$)/`, and the effective method is
  not GET → a write to an issues endpoint. The regex covers `repos/o/r/issues` (create),
  `…/issues/5` (edit, close, lock), `…/issues/5/comments`, `…/issues/5/labels`,
  `…/issues/comments/123`, and the full `https://api.github.com/…` form of each, and does not match
  `…/pulls/5/reviews`, `…/pulls/5/comments`, or `graphql`.
- The subcommand form is the same filter: `issue` positional followed later by one of the twelve
  write verbs (`comment create edit close reopen delete pin unpin transfer lock unlock develop`).
  Because flag *values* survive the filter as positionals, `issue --repo o/r comment 5` is caught
  (the `--repo` value sits between `issue` and `comment`) — the same mechanism that makes
  `pr --repo o/r merge` refuse.

Three things the filter gets wrong, all accepted and documented in the predicate's comment rather
than fixed with a real parser:

- A flag value equal to a verb (`issue list --search close`) is refused — a GitHub-issue read
  Legion does not perform anyway.
- A non-GET call whose flag value contains `/issues/` (a `-f body=…/issues/…`) is refused.
- pflag-combined shorthands (`-if body=x`, `-iXPOST`) and `-X=GET` are not decoded: the first
  forwards, the second over-refuses. The hazard being closed is a stale template followed in good
  faith, not a worker evading the guard, so these are not worth a parser.

One endpoint fact worth knowing before you copy the regex: GitHub serves pull-request
**conversation** comments from the issues endpoint (`repos/o/r/issues/<pr>/comments`,
`repos/o/r/issues/comments/<id>`). Reading them is a GET and forwards; editing or deleting one by
raw API is refused by this rule, and `gh pr comment --edit-last` / `--delete-last` (a `pr`
subcommand) is the route that stays open. Review comments live under `…/pulls/<n>/comments` and are
untouched.

## Pinning it

The tests (`packages/daemon/src/cli/__tests__/index.test.ts`, `describe("legion gh")`) are two
`it.each` tables plus the named cases, and their rows are the contract, not the implementation:

- refused: `-X POST …/issues/5/comments -f body=hi`; `…/issues -f title=x` (POST implied);
  `…/issues/5/comments --input body.json`; `--method=PATCH …/issues/5 -F state=closed`;
  `-XDELETE …/issues/comments/99`; `-X PUT https://api.github.com/…/issues/5/lock` — each with
  the token fetch and the `gh` spawn both never called;
- forwarded, argv unchanged and token fetched: `pr comment`, `pr review`,
  `api --method POST …/pulls/5/reviews --input body.json`, `api graphql -f query=mutation …`,
  `issue view`, `issue list`, `api …/issues/5/comments --jq length`,
  `api --method GET …/issues -f state=open`.

A rewrite of the body-flag condition that drops `--field` or `--raw-field` would pass this suite
today; add rows for a flag before you rely on it (the LEGION-78 fast-follow adds those two).

## The static table

The verb table is a `Record<string, true>` tested with `Object.hasOwn`, not indexed: an argv token
such as `constructor` or `toString` is truthy through the prototype on a plain object index. A
`Set` is equally conventional in this repository (`SKIP_DIRS` in `index/graph.ts`); the repository
rule prefers a `Record` for a static string table (`FAILING_CHECK_CONCLUSIONS` in
`state/fetch.ts`), and `hasOwn` is the part that matters for a table looked up with untrusted keys.
