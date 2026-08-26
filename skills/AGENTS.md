# Skills Layer

Legion skills guide the architect and its sequential phase workers in a shared issue workspace.
They are Markdown instructions loaded by Oh My Pi sessions; the daemon and OMP extension own
event intake, process lifecycle, credentials, and role delivery.

## Structure

```
skills/
├── github/              # GitHub issue and pull-request operations
├── linear/              # Linear adapter instructions
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

Workers do not run a controller loop, mutate lifecycle labels, or create human dispatch threads.
When an issue needs a product, scope, cross-phase, or human decision, they send the owning
architect the verified observation and decision needed through hub.

## Durable artifacts

Phase handoffs are committed in lifecycle order: architect, plan, implement, test, and review.
The reviewer removes `.legion/` as its final commit; retro records its learning in
`docs/solutions/` and writes no handoff. GitHub comments and reviews carry the required Legion
footer so the daemon can attribute artifacts to their worker session.
