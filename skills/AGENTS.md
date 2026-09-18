# Skills Layer

Legion skills guide the architect and its sequential phase workers in a shared issue workspace.
They are Markdown instructions loaded by Oh My Pi sessions; the daemon and OMP extension own
event intake, process lifecycle, credentials, and role delivery.

| Skill | Who reads it | What it owns |
| --- | --- | --- |
| `dispatch/` | every role, and any session writing to Dispatch | specs, asks, comments, artifacts, and messages on native Dispatch |
| `envoy/` | every role | subscriptions, agent-to-agent messages, and topic formats |
| `legion-architect/` | root and sub-architects | tree ownership, decomposition, waves, gates, integration, sign-off |
| `legion-controller/` | the controller root process | wake routing, backlog admission, escalation |
| `legion-oracle/` | any role doing research | repository-grounded research |
| `legion-retro/` | the implementer, at retro | the pre-merge retrospective and its Dispatch message |
| `legion-worker/` | planner, implementer, tester, reviewer, merger | the phase contracts: handoffs, GitHub identity, PR body and READY discipline, the merge-gate order |

The owning skill above is where each contract is defined; a role prompt that needs a contract from its own seat points there or restates only its own step. This file lists and does not restate.
The text a worker boots with (its role prompt) lives in `packages/pi-envoy/roles/` and is
composed per role in `packages/daemon/src/daemon/processes.ts`; `packages/pi-envoy/roles/roles.test.ts`
holds the structural rules for those parts.
