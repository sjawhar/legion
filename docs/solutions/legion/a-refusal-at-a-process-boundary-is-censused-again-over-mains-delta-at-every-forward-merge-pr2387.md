---
title: "A refusal at a process boundary is censused again over main's delta at every forward merge"
category: legion
tags:
  - contract-census
  - forward-merge
  - cli-refusal
  - required-check
date: 2026-10-30
status: active
module: packages/envoy/cmd/dispatch
related_issues:
  - "sjawhar/legion#2387"
---

# A refusal at a process boundary is censused again over main's delta at every forward merge

Extends docs/solutions/daemon/a-forward-merge-brings-mains-new-outbox-writer-in-unscoped-the-commit-scope-census-runs-again-and-the-racy-wakes-error-line-is-the-tests-witness-LEGION-694.md.

That note re-runs a call-site census inside one package after a forward merge. A refusal on
something other code starts as a process - a command that now needs an environment variable, a
binary that rejects a flag - has callers in other modules' tests, which a package-scoped grep never
reaches.

- After each forward merge, re-run every search command of the pull request's contract census over
  the whole repository, tests included, not only the package that refuses.
- A hit main added since the census was written arrives with no conflict marker and can turn a
  required job red. Fix it in this pull request (the caller supplies what the refusal asks for) and
  add it to the census with its disposition and the commit that fixed it.
- When the caller is a test that starts the real command, give it what a deployment gives: for a
  command that needs a migrated database and migrates nothing itself, a database of the test's own,
  brought to the schema by a subcommand that does migrate.

## Evidence

`envoy-dispatch redeliver-webhooks` began refusing to start without `DATABASE_URL`
(`packages/envoy/cmd/dispatch/redeliver.go`). After the census was written, main added
`packages/daemon/internal/intake/redelivered_test.go`, which runs the real command with no
`DATABASE_URL`; the merged tree's required `test` job failed with `exit status 2 …
redeliver-webhooks: DATABASE_URL is required`. The test now takes `testpg.Database`, migrates it
with `envoy-dispatch backfill-block-ids` (which migrates and then finds nothing to do on an empty
database; `rebuild-refs` needs a server URL), and passes `DATABASE_URL`. Pull request
sjawhar/legion#2387.
