# Skills Layer

Legion skills guide the architect and its sequential phase workers in a shared issue workspace.
They are Markdown instructions loaded by Oh My Pi sessions; the daemon and OMP extension own
event intake, process lifecycle, credentials, and role delivery.

| Skill | Who reads it | What it owns |
| --- | --- | --- |
| `ce-simplify-code/` | the implementer, once per pull request | the behaviour-preserving simplify pass before the reviewer's final pass (Legion's copy of the MIT-licensed Compound Engineering skill; `LICENSE` beside it) |
| `dispatch/` | every role, and any session writing to Dispatch | specs, asks, comments, artifacts, and messages on native Dispatch |
| `envoy/` | every role | subscriptions, agent-to-agent messages, and topic formats |
| `legion-architect/` | root and sub-architects | tree ownership, decomposition, waves, gates, integration, sign-off |
| `legion-controller/` | the controller root process | wake routing, backlog admission, escalation |
| `legion-oracle/` | any role doing research | repository-grounded research |
| `legion-retro/` | the implementer, at retro | the pre-merge retrospective and its Dispatch message |
| `legion-worker/` | planner, implementer, tester, reviewer, merger | the phase contracts: handoffs, GitHub identity, PR body and READY discipline, the merge-gate order |
| `thermonuclear-code-quality/` | the `thermonuclear-code-quality` agent | the maintainability rubric of the reviewer's pair |
| `thermonuclear-deep-review/` | the `thermonuclear-deep-review` agent | the security and correctness rubric of the reviewer's pair |

The owning skill above is where each contract is defined; a role prompt that needs a contract from its own seat points there or restates only its own step. This file lists and does not restate.
A Legion prompt (a skill here, a role prompt, or an agent definition in `packages/pi-envoy/agents/`) names a task agent only as `task(agent="<name>")` and a skill it tells the model to load only as `skill://<name>`. Those are the two forms the Go daemon's boot gate and `legion probe-image` resolve through Oh My Pi, refusing by name one it cannot find; a dispatch or a load written any other way goes unchecked. An agent or skill Legion's prompts name is shipped here or in `packages/pi-envoy/agents/`, unless Oh My Pi bundles it.
The text a worker boots with (its role prompt) lives in `packages/pi-envoy/roles/` and is
composed per role in `packages/daemon/src/daemon/processes.ts`; `packages/pi-envoy/roles/roles.test.ts`
holds the structural rules for those parts.
