---
title: "When one tool's result feeds the next call, name the exact field, print its value in the result text, and make the receiver refuse the wrong kind: the slug-for-UUID gate that every unit suite passed"
category: skill-patterns
tags:
  - skill-authoring
  - tool-result-shape
  - identifiers
  - register_gate
  - dispatch_request_approval
  - real-surface-defect
date: 2026-09-13
status: active
module: skills/legion-architect, packages/pi-envoy/src/legion/tools.ts, packages/contracts/src/legion-daemon-api.ts, packages/envoy-client/src/dispatch-execute.ts
related_issues:
  - "LEGION-20"
  - "sjawhar/legion#975"
---

# When one tool's result feeds the next call, name the exact field, print its value in the result text, and make the receiver refuse the wrong kind

## What went wrong

The architect skill's approval sequence was two calls: `dispatch_request_approval({ issue,
artifact: "spec" })`, then `legion({ op: "register_gate", issue, artifactId: <the document>,
version })`. The first call's *input* names the document by slug or file name (`spec`,
`spec.md`); the daemon's gate matches Dispatch's `artifact.approved` events on the document's
UUID. The first version of the skill used one word, "the artifact", for both, and its result text
did not print the id. On the smoke rig the real architect registered `artifactId: "spec"`. Every
layer reported success — the tool accepted a non-empty string, the daemon stored it, the state
read-out showed a gate — and no approval event could ever match it, so the gate could never open.

Every unit suite passed. The reducer tests fed the correct UUID; the route tests sent the correct
UUID; nothing modelled a model reading a result and copying the wrong noun into the next call.
The implementer's pre-merge end-to-end run found it (PR #975, commit `97b0b3fd` "register_gate
takes the document id — a slug can never open the gate").

## The fix is three layers, not one

1. **The result text prints the value the next call needs, labelled.** `dispatch_request_approval`
   now answers `Approval requested for spec.md (document id <UUID>) at version N …`, and its
   `details.artifact` / `details.version` carry the same two values. The "already approved"
   branch prints the document id too, because an architect reaches it when a human approved
   before the request and still has to register.
2. **The skill names the exact field and shows a specimen.** Section 1 of the architect skill
   copies `result.details.artifact` and `result.details.version`, shows a UUID literal, and says
   in words that the id is never the slug or file name you passed in.
3. **The receiver refuses the wrong kind, naming the field.** The daemon contract's
   `GatesRegister.request.artifactId` is `z.uuid()`; the `legion` tool checks the same schema
   before the round trip and fails with a message that says where the id comes from; the daemon
   lowercases the id on the way in because Dispatch emits its UUIDs lowercase and the reducers
   compare with `===`.

   The tool's check must be *the contract's own schema*, not a look-alike. The first version of
   the tool carried its own UUID regex, which accepted any hex digit in the version and variant
   positions while the contract's `z.uuid()` checks the RFC 4122 bits — so an id could pass the
   tool and be refused by the daemon. The reviewer caught it; the tool now calls
   `LegionDaemonApi.GatesRegister.request.shape.artifactId.safeParse(value)`. A value validated
   at two layers references one definition, or the two drift while each looks correct alone.

Any one layer alone would have left a hole: text without a contract lets a retyped slug through;
a contract without text sends the architect a 400 with no idea where the right value lives.

## The reusable rule

Whenever a skill chains two tool calls and the second's argument comes from the first's result:

- write the field path (`result.details.<name>`) in the skill, not a noun;
- make the first tool's human-readable text print that value next to its label, because the
  model reads the text before the JSON;
- give the receiving schema a shape that the *wrong* value cannot satisfy (UUID vs. slug, id vs.
  number, path vs. name), so a copy mistake is a 400 that names the field, never a stored value
  nothing will ever match;
- test the chain once on the real surface. A suite that hands both calls the right value proves
  the plumbing, not the hand-off.

## Related

- `docs/solutions/testing/scratch-daemon-rig-proves-what-unit-tests-cannot.md` — the class of
  defect only a real run finds.
- `docs/solutions/daemon/omp-append-system-prompt-is-last-wins-join-the-fragments.md` — the other
  real-surface-only defect from the same pull request.
