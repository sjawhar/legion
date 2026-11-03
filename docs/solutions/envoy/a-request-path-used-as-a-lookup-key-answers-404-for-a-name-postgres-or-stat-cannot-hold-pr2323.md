---
title: "A request path used as a lookup key answers 404 for a name Postgres or stat cannot hold"
category: envoy
tags:
  - postgres
  - utf8
  - static-files
  - error-handling
date: 2026-10-28
status: active
module: packages/envoy
---

# A request path used as a lookup key answers 404 for a name Postgres or stat cannot hold

The API's input layer refuses text Postgres cannot store (`text.Storable`, `api/storable_text.go`),
but a key taken from a URL path never passes through it. Before such a key reaches a store or the
filesystem:

- Check it with `text.Storable` and answer a refused key as not found. Postgres refuses a `text`
  parameter holding a NUL byte or invalid UTF-8 with SQLSTATE 22021 rather than finding no row,
  so without the check a mistyped path is a 502 and an ERROR log line that anyone can trigger.
  Put the check in the store method (only it knows its key is a text parameter), and in the
  handler too when the handler reserves memory or a connection before asking.
- Read `os.Stat`'s `EINVAL` (a NUL byte), `ENAMETOOLONG` and `ENOTDIR` as "no file there", the same
  as `fs.ErrNotExist`. Keep a 500 for the errors that mean the filesystem failed (`EACCES`, `EIO`).
- Test with real Postgres, not a fake store: the fake answers "not found" for any key, so the 502
  never shows.

## Evidence

Pull request #2323, `packages/envoy/internal/dispatch/routes/router.go:528` (the handler guard),
`:543` (`isMissingFile`) and `packages/envoy/internal/dispatch/dashboard/dashboard.go:317` (the
store guard). Before `599232ec`, `GET /assets/foo%00.js` and six other refused paths answered
500 `stat failed`; before `fc316304`, `GET /assets/%ff.js` answered 502 with `invalid byte sequence
for encoding "UTF8": 0xff (SQLSTATE 22021)`. `TestGetAssetOfAKeyNoTextCanHoldIsNotFound`
(`dashboard_test.go:155`) runs on real Postgres.
