---
title: "Every fallible call that touches a secret sits inside the error handling, and the refusal carries neither the driver's message nor a cause"
category: best-practices
tags:
  - secrets
  - error-handling
  - connection-string
  - bun-sql
  - refusal-messages
  - review-findings
date: 2026-09-14
status: active
module: sjawhar/oh-my-pi packages/coding-agent/src/session/session-storage-config.ts (consumed by packages/daemon via OMP_FORK_PIN)
related_issues:
  - "LEGION-80"
  - "sjawhar/legion#1081"
symptoms:
  - 'TypeError: "postgres://host=db user=legion password=hunter2" cannot be parsed as a URL.'
  - "a refusal that names the variable and the path on five branches, and prints the whole connection string on the sixth"
---

# Every fallible call that touches a secret sits inside the error handling

## What happened

The fork resolver that turns `OMP_SESSION_SQL_DSN_FILE` into a `SqlSessionStorage` had five
carefully worded refusals — missing file, blank file, no file named, unknown storage value,
unreachable database — each naming the variable or setting and the path and never the file's
contents. Between the file read and the `try` around `SqlSessionStorage.create()` sat one line
that looked infallible:

```ts
const client = new SQL(dsn);
```

`Bun.SQL` parses the string in its constructor. A libpq keyword form
(`host=db user=legion password=hunter2`) is not a URL, so the constructor threw
`TypeError ERR_INVALID_URL` with the **whole string in its message**. That error was not the
resolver's own error class, so the CLI's catch rethrew it and the process-level `fatal` handler
wrote `Bun.inspect(error)` to stderr and to OMP's log — password included. Every unit test
passed; the review found it by reading the code (review round 1 of PR #1081, thread
`discussion_r4003047466`).

## The pattern

1. **Enumerate every call that can throw while holding the secret**, including the ones that
   "cannot fail": constructors, parsers, `new URL()`, `JSON.parse`. The leak sat on the one call
   nobody thought of as fallible.
2. **Construct inside the handling**, and refuse with a message you wrote:

   ```ts
   let client: SQL;
   try {
     client = new SQL(dsn);
   } catch {
     throw new SessionStorageConfigError(
       `${source} names ${dsnFile}, but its contents are not a connection URL the database driver accepts`,
     );
   }
   ```

   No interpolation of the driver's message (it embeds the string) and **no `cause`**: the fatal
   handler prints `Bun.inspect(error)`, which renders the cause chain, so a cause would carry
   the secret out by another door. Name the variable or setting and the path; say what class of
   thing the contents failed to be; stop.
3. **Test the surface the fatal handler prints, not `error.message`.** The regression test
   writes a file containing `password=hunter2`, asserts the exact refusal message, and asserts
   `expect(Bun.inspect(error)).not.toContain("hunter2")`. A test on `error.message` alone would
   pass with a `cause` attached and the leak intact. Apply this to every refusal branch — the
   sibling "unreachable database" refusal in the same resolver still attaches `cause` and its
   test still asserts only `error.message` (tracked as a pre-upstream item on the fork); the
   class is not fully closed until every branch has the `Bun.inspect` assertion.
4. **Prove it on the binary, both ways.** On the fixed build: exit 1, the refusal on stderr,
   `grep -c hunter2` over stdout and stderr → 0. On the previous build, the same file → the
   `TypeError` with the password on stderr. The negative control is what shows the fix is doing
   the work rather than the test being vacuous.

## Why the design phase missed it

The plan named the refusals from the spec's Errors table (missing / unreadable / blank /
unreachable / unknown value). "Contents that are not a URL" was not a row, so no test targeted
it, and the resolver's shape — read, trim, construct, connect — put the construction in the
gap between two guarded regions. When a spec's error table is the source of the branches,
walk the code's fallible calls against it and add the rows the code has that the table does not.
