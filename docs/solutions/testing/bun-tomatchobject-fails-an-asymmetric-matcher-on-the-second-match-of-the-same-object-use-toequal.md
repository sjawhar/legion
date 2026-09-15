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

Provisioning hands its clone and its fetch **one shared env object** (`credential.env` in
`createProvisioningCredential`). A test that asserts both commands received it, with an
asymmetric matcher for the path that differs per run, fails on the second assertion:

```ts
const expected = () => ({ GIT_ASKPASS: expect.stringContaining(`${root}/`), GIT_CONFIG_COUNT: "2" });
expect(clone.opts.env).toMatchObject(expected());   // passes
expect(fetch.opts.env).toMatchObject(expected());   // fails: "- Expected - 0 / + Received + 0"
```

Minimal reproduction on Bun 1.3.14 (`bun test v1.3.14 (0d9b296a)`): the same object reference
matched twice with `toMatchObject` and `expect.stringContaining` fails the second time; with
plain values only it passes twice; a fresh `expected()` per call makes no difference — the
**received** object's identity is what trips it. The reviewer reproduced the same on their run.

## The fix

`toEqual` with the asymmetric matcher has no such quirk and asserts the exact shape, which is
the contract anyway (an extra `GIT_CONFIG_KEY_2` without a bumped count is silently ignored by
git, so an exact key set is a real assertion, not pedantry):

```ts
const provisioningEnv = {
  GIT_ASKPASS: expect.stringContaining(`${root}/`),
  GIT_TERMINAL_PROMPT: "0",
  LEGION_PROVISIONING_TOKEN: "ghs_x",
  GIT_CONFIG_COUNT: "2",
  GIT_CONFIG_KEY_0: "credential.helper",
  GIT_CONFIG_VALUE_0: "",
  GIT_CONFIG_KEY_1: "credential.interactive",
  GIT_CONFIG_VALUE_1: "true",
};
expect(commands[0]?.opts?.env).toEqual(provisioningEnv);
expect(fetchCommand?.opts?.env).toEqual(provisioningEnv);
```

This is the shape `processes.test.ts` already used for the same env (`toContainEqual` with
`expect.stringMatching` on `GIT_ASKPASS`). The round-1 workaround — `toMatchObject` on plain
values plus a separate `toStartWith` on `GIT_ASKPASS` — worked but split one contract over two
assertions and lost the exact-key-set check; the review folded it into the `toEqual` above.

## When you hit an empty-diff failure

An `expect` failure whose diff shows nothing changed is a matcher problem, not a data problem.
Dump the received objects (both were byte-identical here), then reduce to a three-line test in a
scratch directory — not under the package, where `bun test` would pick it up — before changing
the code under test.
