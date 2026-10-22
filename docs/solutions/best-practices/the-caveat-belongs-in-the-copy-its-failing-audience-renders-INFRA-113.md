---
title: "The caveat belongs in the copy its failing audience renders"
category: best-practices
tags:
  - documentation
  - agent-facing-text
  - tool-contracts
  - review
date: 2026-10-22
status: active
module: envoy
related_issues:
  - "INFRA-113"
  - "sjawhar/legion#2105"
---

# The caveat belongs in the copy its failing audience renders

When a behaviour's exception is a property of the host, the caveat belongs in the text that host
renders. A correct paragraph in a skill the host never loads does not count, however close it is
to the one the host reads.

Before you edit a claim, enumerate its copies: grep the claim's most distinctive phrase, list every
file that carries it, and for each one name the host that renders it and whether that host is
inside or outside the exception. Copies inside the exception must carry the caveat. Copies outside
it may be left — but only as a decision taken after the enumeration, never as the copies you
happened not to open.

A sentence that draws a contrast names an axis. Drive one example on each side of the axis you
chose, and one on the axis you rejected. A wrong axis collapses in a single call.

## Evidence

INFRA-113 made an ad-hoc Envoy topic travel over core NATS. The delivery claim lives in five
places. `skills/envoy/SKILL.md` and `packages/envoy/AGENTS.md` carried the exception;
`packages/envoy-client/src/tool-contract.ts` and `docs/site/src/content/docs/dispatch/envoy.md` did
not — and those two are exactly what the exception's victim reads.
`packages/envoy-plugin/src/server.ts` renders that tool description verbatim to an OpenCode agent
and registers that session with a serve port and no `selfSubscribed`, so
`packages/envoy/internal/session/session.go` reaches it only by push; and no ad-hoc topic enters a
push path at all, because the listener's one core subscription is `notifications.role.>`
(`packages/envoy/cmd/listener/main.go`) and its durable filters to `bus.StreamSubjects()`
(`packages/envoy/cmd/listener/durable.go`). Such an agent subscribes — accepted, no warning —
publishes, gets 200, and receives nothing for ever, with no delivery exception and no log line,
while reading a sentence that told it "yours included".

Measured rather than reasoned, on a rig with that subscriber added: the ad-hoc publish answered
`http=200` and the serve-port session was pushed nothing, with no exception line in the listener's
log; in the same run a stream-captured topic *was* pushed to that same session, which is what made
the silence a result rather than a broken harness.

The axis half is the round before it. The first corrected paragraph said self-delivery turns on
whether a topic is built-in or ad-hoc. The real axis is where the session takes its deliveries. One
`envoy_publish` from a subagent to a built-in topic its parent subscribed to — which the parent
received — collapsed the rejected axis, and the control on the role lane, which answered
`Not delivered`, placed the real one.

Both defects were found by driving the surface, in a phase whose business was prose. Two of this
issue's three review and test rounds were spent on agent-facing text that overstated delivery, and
neither round's finding was visible by reading.

## When to apply

Any claim about behaviour that appears in more than one place, and especially any
`@legion/contracts` tool description: those strings are rendered to agents on hosts whose
behaviour differs, and a tool description is read alone, never beside the skill that qualifies it.

## Related

- `docs/solutions/legion/text-written-to-close-a-finding-is-not-exempt-from-the-rule-the-finding-was-about-INFRA-69.md`
  — re-run a finding's own criterion against the sentence written to close it.
- `docs/solutions/legion/committed-plugin-bundles-inline-contracts-so-every-contracts-change-carries-a-dist-commit.md`
  — the tool description is inlined into a committed bundle, so correcting it carries a rebuild.
