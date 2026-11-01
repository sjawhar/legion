---
title: "Bun 1.3.14's toMatchObject fails an asymmetric matcher on the second match of the same received object; assert an exact shape with toEqual and the matcher instead"
category: testing
tags:
  - bun-test
  - tomatchobject
  - toequal
  - asymmetric-matcher
  - expect-stringcontaining
date: 2026-09-15
status: active
module: packages/daemon/src/cli/__tests__/workspace-init.test.ts, packages/workspace/src/workspace.test.ts
related_issues:
  - "LEGION-178"
  - "sjawhar/legion#1121"
symptoms:
  - "`expect(x).toMatchObject({...})` fails with an empty diff (`- Expected - 0 / + Received + 0`) on the second of two identical assertions"
  - "the same assertion passes when the received object is a fresh copy"
---

# Bun 1.3.14's `toMatchObject` fails an asymmetric matcher on the second match of the same object

## The quirk

A test that asserts the same received object twice with `toMatchObject` and an asymmetric
matcher fails the second time, with a diff that shows nothing changed. It bit LEGION-178 when two
commands were handed one shared env object and the test checked each command received it. The
whole quirk fits in one test, and this reproduces it on the repository's pinned Bun (1.3.14):

```ts
const env = { HOME: "/home/legion/workspace", MODE: "fetch" };
const expected = () => ({ HOME: expect.stringContaining("/home/legion/"), MODE: "fetch" });
expect(env).toMatchObject(expected());   // passes
expect(env).toMatchObject(expected());   // fails: "- Expected - 0 / + Received + 0"
```

With plain values only it passes twice, and a fresh `expected()` per call makes no difference.
The cause is that Bun's `toMatchObject` **writes the matcher into the received object**: after
the first assertion `env.HOME` is the `stringContaining` matcher, not the string
(`JSON.stringify(env)` gives `{"HOME":{},"MODE":"fetch"}`). So every later asymmetric match
against it fails, `toEqual` included; a later plain-valued match is compared against the planted
matcher and passes for any value that matcher accepts — `toEqual({ HOME: "/home/legion/ELSEWHERE",
MODE: "fetch" })` passes, a false green; and anything else that reads the object sees matchers, not
values. `toEqual` leaves the received object as it was.

## The fix

Assert with `toEqual` from the first assertion on. Repeated `toEqual` matches against one object
pass, and it asserts the exact shape:

```ts
const env = { HOME: "/home/legion/workspace", MODE: "fetch" };
expect(env).toEqual(expected());   // passes
expect(env).toEqual(expected());   // passes
```

The exact shape is usually the contract anyway. For an environment handed to `git`, an extra
`GIT_CONFIG_KEY_n` without a bumped `GIT_CONFIG_COUNT` is silently ignored, so an exact key set
is a real assertion, not pedantry. LEGION-178's round-1 workaround — `toMatchObject` on plain
values plus a separate `toStartWith` on the one varying path — worked, but split one contract
over two assertions and lost the exact-key-set check; the review folded it into one `toEqual`.

## When you hit an empty-diff failure

An `expect` failure whose diff shows nothing changed is a matcher problem, not a data problem.
Dump the received objects (both were byte-identical here), then reduce to a three-line test in a
scratch directory — not under the package, where `bun test` would pick it up — before changing
the code under test.
