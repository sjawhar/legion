# Skills Layer

Legion skills guide the architect and its sequential phase workers in a shared issue workspace.
They are Markdown instructions loaded by Oh My Pi sessions; the daemon and OMP extension own
event intake, process lifecycle, credentials, and role delivery.

| Skill | Who reads it | What it owns |
| --- | --- | --- |
| `ce-simplify-code/` | the implementer, once per pull request | the behaviour-preserving simplify pass before the reviewer's final pass (Legion's copy of the MIT-licensed Compound Engineering skill; `LICENSE` beside it) |
| `dispatch/` | every role, and any session writing to Dispatch | specs, asks, comments, artifacts, and messages on native Dispatch |
| `dispatch-first/` | every session with Dispatch configured, injected by each host plugin on every request (Oh My Pi), at session start, `/clear` and `/compact` (Claude Code), or as an instruction file (OpenCode) | searching Dispatch before acting, extending the existing issue, citing decisions, closing duplicates; kept under 60 lines and 6,000 characters |
| `envoy/` | every role | subscriptions, agent-to-agent messages, and topic formats |
| `legion-architect/` | root and sub-architects | tree ownership, decomposition, waves, gates, integration, sign-off |
| `legion-controller/` | the controller root process | wake routing, keeping the admission slots full from `todo`, the daily report, escalation |
| `legion-oracle/` | any role doing research | repository-grounded research |
| `legion-retro/` | the implementer, at retro | the pre-merge retrospective and its Dispatch message |
| `legion-worker/` | planner, implementer, tester, reviewer, merger | the phase contracts: handoffs, GitHub identity, PR body and READY discipline, the merge-gate order |
| `thermonuclear-code-quality/` | the `thermonuclear-code-quality` agent | the maintainability rubric of the reviewer's pair |
| `thermonuclear-deep-review/` | the `thermonuclear-deep-review` agent | the security and correctness rubric of the reviewer's pair |

The owning skill above is where each contract is defined; a role prompt that needs a contract from its own seat points there or restates only its own step. This file lists and does not restate.
A Legion prompt (a skill here, a role prompt, or an agent definition in `packages/pi-envoy/agents/`) names a task agent only as `task(agent="<name>")` and a skill it tells the model to load only as `skill://<name>`. Those are the two forms the Go daemon's boot gate and `legion probe-image` resolve through Oh My Pi, refusing by name one it cannot find; a dispatch or a load written any other way goes unchecked. An agent or skill Legion's prompts name is shipped here or in `packages/pi-envoy/agents/`, unless Oh My Pi bundles it.
A skill file an agent loads stays under 51,200 bytes, Oh My Pi's spill threshold (a longer one arrives with its middle cut out), and a skill's detail lives in `references/` files linked as `skill://<name>/references/<file>.md`, never by relative path, which a read by path cuts at 300 lines. `packages/pi-envoy/src/skills-guard.test.ts` fails on a file at the threshold, on any `SKILL.md` body of 500 lines or more, on a skill whose frontmatter `name` is not its directory's name, on a `dispatch-first` over its budget, on a `skill://<name>/<path>` link that names a missing file or a missing `#heading`, and on a `legion-worker` reference nothing links.
The text a worker boots with (its role prompt) lives in `packages/pi-envoy/roles/` and is
composed per role in `packages/daemon/src/daemon/processes.ts`; `packages/pi-envoy/roles/roles.test.ts`
holds the structural rules for those parts.
