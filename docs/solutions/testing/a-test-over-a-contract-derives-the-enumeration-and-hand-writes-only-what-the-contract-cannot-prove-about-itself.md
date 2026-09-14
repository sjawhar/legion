---
title: "A test over a contract derives the enumeration from the contract, hand-writes only what the contract cannot prove about itself, and audits everything behind a red gate assertion"
category: testing
tags:
  - contract-drift
  - dispatchToolSpecs
  - envoy-plugin
  - required-arguments
  - toJSONSchema
  - non-exhaustive-map
  - gate-assertion
  - masked-assertions
date: 2026-09-14
status: active
module: packages/envoy-plugin
related_issues:
  - "LEGION-96"
  - "sjawhar/legion#1086"
symptoms:
  - "bun test packages/envoy-plugin/src/__tests__/dispatch-tools.test.ts: expect(received).toEqual(expected) Received + 2 naming dispatch_request_approval and dispatch_open_asks"
  - "the same test, once the names pass: dispatch_ask required ['question'] received where ['issue','question'] was expected"
---

# A test over a contract derives the enumeration from the contract, hand-writes only what the contract cannot prove about itself, and audits everything behind a red gate assertion

`packages/envoy-plugin/src/server.ts` registers one OpenCode tool per entry of
`dispatchToolSpecs` (`@legion/contracts`): `for (const spec of dispatchToolSpecs) { dispatchTools[spec.name] = tool({ args: spec.arguments(zodSchemaApi(tool.schema)), … }) }`.
Its test, `src/__tests__/dispatch-tools.test.ts`, hand-typed the twelve tool names it expected
together with each tool's required arguments in one `as const` tuple array. The contract then gained
`dispatch_request_approval` and `dispatch_open_asks`; the plugin registered fourteen tools; the
test's first assertion went red with `Received + 2` and stayed red on `main` — nothing ran it (see
the CI half of this issue in
[a-package-no-workflow-names-has-no-ci](a-package-no-workflow-names-has-no-ci.md)). Nothing was
wrong with the plugin.

## Split the test along what the contract can and cannot prove

The test asserts two different things, and they have different sources of truth.

**Which tools exist** is the contract's own fact. The plugin loops over `dispatchToolSpecs`; the
only thing worth checking is that every entry came out the other side and nothing else did. Read
it from the same export production reads, in the same order (`server.ts` fills `dispatchTools` in
contract order and spreads it first into `hooks.tool`, so `Object.keys` insertion order is the
contract's order and an ordered `toEqual` is right):

```ts
import { dispatchToolSpecs } from "@legion/contracts";

expect(Object.keys(tools).filter((name) => name.startsWith("dispatch_"))).toEqual(
  dispatchToolSpecs.map((spec) => spec.name)
);
```

A tool added to the contract now passes without a test edit; a tool the plugin drops, or one it
invents, fails with the diff naming it (proven both ways: `if (spec.name === "dispatch_search") continue;`
in the loop → `- "dispatch_search"` under `Expected - 1`; `"dispatch_bogus"` appended to the
expected list → `- "dispatch_bogus"`).

**Which arguments each registered tool requires** is *not* the contract's fact to prove: it is the
output of the adapter under test — `zodSchemaApi(tool.schema)` building OpenCode's Zod from the
contract's field shapes, read back through `tool.schema.toJSONSchema(...).required`. Deriving that
from the contract too would compare the adapter with itself and catch nothing. Keep it hand-written,
one entry per tool, and make the loop read the map — not the contract — so a tool the map does not
name is registered but not asserted:

```ts
const requiredArguments: Record<string, readonly string[]> = {
  dispatch_issue: ["project", "title"],
  dispatch_ask: ["question"],
  // …
  dispatch_request_approval: [],
  dispatch_open_asks: [],
};

for (const [name, required] of Object.entries(requiredArguments)) {
  const registered = tools[name];
  expect(registered).toBeDefined();               // a typo'd key fails; it does not pass on undefined
  const schema = tool.schema.toJSONSchema(tool.schema.object(registered.args), { io: "input" });
  expect(schema.required ?? []).toEqual(required);
}
```

Type the map `Record<string, readonly string[]>`, **not** `Record<(typeof dispatchToolSpecs)[number]["name"], …>`.
The exhaustive key type looks stricter, but it makes the next contract tool fail the *plugin's
typecheck* instead of registering silently — the exact regression the derived name list exists to
end (the spec's Rejected list records this so nobody re-proposes it). The cost is the one the spec
accepts: a new tool's required arguments are unasserted until someone adds its line.

## When the gate assertion goes red, audit everything behind it

The name comparison sits before the per-tool loop, so while it was red the loop never ran. The
issue said "two tools missing"; the spec's requirements table said the argument expectations
"gain entries for the two new tools" and otherwise stay. Once the planner made the names pass, the
loop failed on `dispatch_ask`: the contract had made `issue` optional on every tool that can also
address a project document (issue-or-project+artifact, enforced at execution by
`documentOwnerValidation`, not in the argument schema). **Five** of the twelve hand-typed lists
were stale, not zero — `dispatch_ask`, `dispatch_comment`, `dispatch_suggest`, `dispatch_doc_edit`,
`dispatch_artifact` — a drift no red signal had ever reported because the assertion in front of them
had been failing first the whole time.

The habit: a failing assertion that gates later assertions (an equality check before a loop, an
early `toBeDefined`, a length check) has been hiding every assertion behind it for as long as it
has been red. Do not fix the line the failure named and stop. Dump the current actual values of
everything behind the gate and compare them to the expectations *before* editing:

```ts
// throwaway, in the test or a bun -e script after initPlugin()
for (const [name, t] of Object.entries(tools)) {
  if (!name.startsWith("dispatch_")) continue;
  const s = tool.schema.toJSONSchema(tool.schema.object(t.args), { io: "input" });
  console.log(name, JSON.stringify(s.required ?? []));
}
```

Then check each difference against the contract source (`packages/contracts/src/dispatch-tools.ts`:
`issue: … .optional()` on each of the five) so the corrected list is the contract's truth, not
"whatever the adapter printed". The planner did exactly this, reported the five stale lists to the
architect by `envoy_publish`, and the spec's requirements table was versioned to name them before
the implementer started — the routing in
[out-of-spec-findings-go-to-the-architect-and-come-back-as-spec-versions](../legion/out-of-spec-findings-go-to-the-architect-and-come-back-as-spec-versions.md);
this document is the test-structure reason the finding existed at all.

## Related

- [fixtures-derive-what-production-derives](fixtures-derive-what-production-derives.md): the same
  principle for a test *helper* that constructs a dependency (a tmux server name) from a literal
  instead of the value production derives. Here the literal was the enumeration itself.
- [a-package-no-workflow-names-has-no-ci](a-package-no-workflow-names-has-no-ci.md): why this red
  test lived on `main` unseen — the package's CI job existed and never ran `bun test`.
