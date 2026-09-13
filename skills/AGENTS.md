# Skills Layer

Legion skills guide the architect and its sequential phase workers in a shared issue workspace.
They are Markdown instructions loaded by Oh My Pi sessions; the daemon and OMP extension own
event intake, process lifecycle, credentials, and role delivery.

## Structure

```
skills/
├── dispatch/            # Writing specs, asks, comments, and artifacts on native Dispatch
├── envoy/               # Envoy subscriptions, agent-to-agent messages, and topic formats
├── legion-architect/    # Tree ownership, decomposition, gates, and scheduling
├── legion-controller/   # Derived-verdict control-plane operation
├── legion-oracle/       # Repository-grounded research
├── legion-retro/        # Post-review retrospective
└── legion-worker/       # Sequential architect, plan, implement, test, and review phases
```

## Phase workers

The extension supplies a phase worker with its issue, workspace, role token, and structured
output schema. The worker claims its supplied role, works only on its phase artifact, and
returns that schema to the architect. It writes the same phase-specific payload to
`.legion/<phase>.json`, verifies it exists, and commits the handoff before reporting completion.
The committed predecessor handoff wins after revival or re-creation.

Workers do not run a controller loop or mutate lifecycle labels. Workers coordinate
lifecycle, scope, and cross-phase decisions with the owning architect by `envoy_publish` to its
role topic, sending the verified observation and decision needed (`hub` reaches only subagents
inside the worker's own process). A worker may call the native `dispatch_*` tools directly for a
durable human question; replies come back to the worker's own session.

## Durable artifacts

Phase handoffs are committed in lifecycle order: architect, plan, implement, test, and review.
Only the implementer pushes them: it and the merger act as the code-writing GitHub App, while
the planner, tester, reviewer, and architects act as the review App (`appRoleForLegionRole`,
`packages/daemon/src/daemon/github-apps.ts`), which holds no `contents` permission — their
handoff commits stay on the shared workspace's issue branch and ride the implementer's next push.
A clean review ends with the `.legion/` deletion pushed by the implementer at the reviewer's
direction, which the reviewer then approves; retro records its learning in
`docs/solutions/` and writes no handoff. GitHub comments and reviews carry the required Legion
footer so the daemon can attribute artifacts to their worker session.
The implement handoff carries the implementer's own production-like proof and the test handoff the
tester's verdict on it plus the tester's own; `legion handoff write` refuses a payload the phase's
schema rejects and names the field. Retro's message goes to the Dispatch issue (`dispatch_message`), never a GitHub issue.
