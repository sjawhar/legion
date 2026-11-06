---
title: "Redact a connection string through the parser that consumes it, and record only the fields you name"
category: testing
tags:
  - credential
  - url-userinfo
  - pgx
  - nats
  - redaction
  - load-test
date: 2026-10-28
status: active
module: packages/envoy/loadtest
related_issues:
  - "sjawhar/legion#2369"
---

# Redact a connection string through the parser that consumes it, and record only the fields you name

Extends docs/solutions/testing/a-proof-script-never-prints-a-dsn-a-userinfo-scrubber-leaks-the-query-and-keyword-password-forms-INFRA-229.md.

That note says to use the store's own parser for error text. This one adds that "compose from
parsed parts" (docs/solutions/daemon/a-configuration-projected-onto-an-unauthenticated-route-is-audited-by-the-loaders-admitted-value-shapes-not-by-field-names-LEGION-791.md)
holds only when the parts come from the consumer's parser. `net/url` is not that parser for a
Postgres or NATS value.

- **Parse with the library that will use the value.** For a Postgres URL that is
  `pgconn.ParseConfig` (pgx v5). For a NATS server list it is nats.go's own split. Never decide
  what a credential-bearing value holds with `url.Parse` and then re-serialize the `url.URL`.
- **Record an allowlist, not "the input minus the password".** Build the recorded form from the
  parsed config's non-secret fields alone (for `-pg`: `postgres://` + each `host:port` + `/` +
  database). Do not write user, password, `sslpassword` or any other parameter. When the parse
  fails, record `<redacted>`.
- **Accept only what that parser reads as you expect.** pgx sends a value without a
  `postgres://` or `postgresql://` prefix (case-sensitive) to its keyword/value parser. Refuse
  such a value, and refuse what `ParseConfig` refuses, before any connection.
- **A refusal names the flag and the reason, never the parser's error.** pgx's parse error masks a
  password with a best-effort pattern, and keeps part of it.
- **Split a list only where its consumer splits it.** nats.go splits a `-nats` value at commas, and
  each server has its own userinfo. A user name alone is the NATS token. pgx reads a comma inside a
  password or a `password=` parameter as part of it, and a host list as one host field.
- **Plant the inputs on which the two parsers disagree.** A sweep with one plain `user:pass@`
  passes the broken redactor. Plant all of these: `postgres://u:#A@h/db`, `postgres://u:?A@h/db`,
  `postgres://u:A,B@h/db`, `postgres://h/db?password=A;B`, `postgres://h/db?password=A#B`,
  `postgres://h/db?sslpassword=A`, and `POSTGRES://u:A@h/db`. Grep every step file and output for
  A and B. As the negative control, run the previous redactor with `go test -overlay`.
- **A planted value holds a character that is not a digit, or more than ten digits.** The output
  you grep holds the machine's own text too, and one part of it changes from run to run: a
  temporary directory's random name, which `os.MkdirTemp` writes as a `uint32` in decimal, so ten
  digits at most. A planted `123` matches it by chance, and the sweep then reports a leak that is
  not there; `TestAForeignRunsMetaHoldsNoTokenOrPassword` failed that way at random on any pull
  request. Where the planted value must stay digits, because digits are what the broken parser
  reads as a port, make it eleven digits or more instead of shortening it.

## Evidence

sjawhar/legion#2369 recorded `loadgen run`'s flags in `meta.json`. Each round of review found a
new leak, because each fix used a parser other than the consumer's:

- `5edd1f88` stripped only the password through `url.Parse`. The NATS user was kept, and a NATS
  user alone is the token.
- `f93dbc53` dropped the whole userinfo from every comma-separated element of every URL flag. A
  comma inside a `-pg` password then leaked: `postgres://u:123,abc@h/db` was recorded as
  `postgres://u:123,h/db`.
- `6eeea895` split only `-nats` and parsed `-pg` whole with `url.Parse`. `postgres://u:#secret@h/db`
  was recorded as given, because `url.Parse` reads `#secret@h/db` as a fragment. `?`, `;` and
  `sslpassword` leaked in the same way, and `POSTGRES://` reached pgx's keyword parser, whose error
  went into `meta.json`.
- `a75717b5` added `parsePG` and `redactPG` (`packages/envoy/loadtest/guard.go:200-224`), which
  refuse, accept and record `-pg` only through `pgconn.ParseConfig`. The six values and the two
  schemes became cases of `TestAForeignRunsMetaHoldsNoTokenOrPassword` and
  `TestGuardPrintsNoURLPassword`. Both tests fail on 6eeea895's `guard.go` and `run.go`. Eight
  live foreign steps left no fragment in any file or output.

A residual noted at review round 7: a password that holds an unescaped `@` can leave part of the
password in the host that pgx parses. An allowlist of fields is only as clean as the consumer's
parse of malformed input.
