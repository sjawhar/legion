---
title: "A proof script never prints a DSN: a userinfo scrubber leaks the query and keyword password forms"
category: testing
tags:
  - secrets
  - dsn
  - transcript
  - live-proof
  - redaction
date: 2026-10-25
status: active
module: scripts/e2e
supersedes: docs/solutions/daemon/a-lenient-wire-field-that-feeds-a-strict-state-schema-is-floored-and-put-on-one-line-where-it-is-normalised-LEGION-663.md
related_issues:
  - "INFRA-229"
  - "sjawhar/legion#2180"
---

# A proof script never prints a DSN: a userinfo scrubber leaks the query and keyword password forms

This supersedes one bullet of the LEGION-663 note: its advice to redact `://user:pass@` to `://***@` is the partial scrubber this rule forbids. The rest of that note stands.

- A script or harness whose output is kept as evidence (a transcript, a proof log, a PR body)
  prints only that the database is configured (`store LEGION_TEST_PG_DSN (set; not printed)`),
  never the DSN through a regex scrubber, however wide.
- pgx also accepts `postgres://user@host/db?password=…` and the keyword form
  `host=… user=… password=…`. A `://user:pass@` pattern prints both forms as they are.
- Go error text that must name the target uses the store's own parser
  (`packages/daemon/internal/store/store.go`, `redactPassword`), which parses the password out and
  redacts both the `password=` and the `:password@` positions.
- Prove the absence by value. Run with a sentinel password in the query form and grep every kept
  evidence file for it; as the negative control, show the old scrubber printing the sentinel.

## Evidence

- `scripts/e2e/merged-root-waits.sh` printed `LEGION_TEST_PG_DSN` through
  `sed 's#://[^@/]*@#://<credential>@#'` into the transcript `transcript_to` keeps. The reviewer
  flagged it as a secret finding (ASVS v5.0.0-16.2.5) and asked for no wider regex.
- After the fix, a run with `…&password=SENTINELpw229` passed all seven steps with 0 evidence files
  holding the sentinel; the old `sed` on that DSN printed it.
