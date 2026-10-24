---
name: Legion and Dispatch
last_updated: 2026-10-24
---

# Legion and Dispatch Strategy

## Legion

### Purpose

Getting work done at a high quality by following agent-builder best practice: putting rigor into the agent development process.

### Positioning

Intelligence where intelligence is needed, and deterministic workflows where it isn't. Agents are not constrained or treated as untrusted: for Legion, an agent is like any other engineer.

### Users

**Primary:** one person who owns an entire product or department, end to end and independently. They hire Legion to get all of that product's or department's work done to a high standard.

## Dispatch

### Purpose

Distilling the conversation: coming to an understanding, so that there is one reliable place where the human's intention and the spec are clear.

### Positioning

Work is relational, so Dispatch is a graph. One piece of work almost always relates to another, and capturing those relationships informs future work, so the same mistakes aren't repeated and the same questions aren't asked again.

### Users

**Primary:** the person taking themselves out of the terminal. They hire Dispatch to focus on the spec and on making their intention clear, and to collaborate with agents (and agents with each other).

## Key metrics

- **Deploy frequency** - changes reaching production per day (DORA).
- **Lead time for changes** - from the first commit to production (DORA).
- **Change failure rate** - share of deployed changes that cause a failure in production (DORA).
- **Time to restore** - from a production failure to its fix (DORA).
- **Cost efficiency** - model spend per delivered change. Legion should not be a waste of money: getting work done through Legion must cost less than asking Claude to do it directly. This is measured; it is never a reason to ask before spending on approved work.

## Tracks

### Specs that aren't garbage

Improve the skills so specs are short, clear and useful: the top priority. *Why it serves the approach:* humans decide what, agents decide how, and the spec is where the "what" lives.

### Work doesn't stall

A worker that stops mid-phase gets prompted again, and a tree never strands its own work. *Why it serves the approach:* one person can own a product end to end only if work moves without them watching.

### Legion works on a real project

A recipe a deployer can roll out at once, so Legion's workers get access to everything the project needs, including its other infrastructure. Starting with Legion working on Legion. *Why it serves the approach:* it is how every stream's work comes to run through Legion.

### Dispatch as a graph

Everything is a node, so verification, handoffs that live in Dispatch, and references to existing knowledge become possible. *Why it serves the approach:* captured relationships stop the same mistakes and the same questions.

## Principles

- **Agents are engineers.** An agent has the minimal privileges it needs and the complete privileges it needs to do its job. Legion does not constrain agents or treat them as untrusted.
- **The daemon is a state machine.** It moves work through its phases and keeps pods alive: start the pod; the pod is down, recreate it. It does not control agents, route their work through itself, make their GitHub calls for them, or decide what a repository's review must be. A repository that wants review criteria sets rules on the repository.
- **Handoffs carry what agents forget, not checks on them.** A handoff tells the next role what it needs, such as the skills the planner found it should use. It is not a guard against misbehaviour and not a limit on what a role may do. A reviewer may write whatever it wants to the pull request: that is how review works.
- **Roles talk to each other.** They are on Envoy for that reason. An implementer with a question messages the planner and gets an answer, without ending a phase.
- **Tested means booted and run.** Agents write unit tests because they are trained to, and that is not enough: until the app has been booted and exercised, it is not tested.
- **Cross review because intelligence is fallible.** An implementer falls for sunk cost and confirmation bias, like anyone. Cross-model-family review is a bias of Legion's, not a statement that agents are misaligned.
- **Pluggable.** Any models; a workflow a team can change to meet its needs; repository-specific prompts.
- **General by default.** Most changes should make Legion more general and move deployment-specific behaviour into the deployment's configuration. Legion holds an opinion about how software engineering is done only where its owner chooses to give it one.
- **Fix the root cause.** No fallbacks, workarounds, or notes warning that something can't be trusted. No protection against a failure nobody has seen; a one-off is not a rule.
- **Climb the enforcement ladder only as far as needed.** When agents fail to do something, first check that it is written down in their instructions and write it if not; then self review; then cross review; then a programmatic gate, for what a program can check easily. Go to the next step only when the one before it measurably failed.
- **Less machinery, one source of truth.** Delete what isn't used, reuse what exists, and keep each fact in one place. Infrastructure, credentials and builds live in infrastructure as code and CI, never on someone's machine.
- **Answer from data.** Rank work by its share of the whole, beat the simplest baseline, and build the key piece before approving a design that rests on it.
- **Tests run before the merge, by machine.** Automated, in CI, enforced by the repository's rules; never a manual or production check handed to a person, and never a circular test.
- **Merged changes reach production on their own**, without downtime.
- **In Dispatch, one element behaves the same everywhere and never blocks the person:** one renderer for mentions and markdown, and no input disabled when the action can be queued.
- **One identity per caller.** Authentication is OAuth or workload identity federation everywhere: a workload exchanges its platform identity for a token, a session acting for a person gets its own token for that person. No shared tokens.
- **Standard best practice first.** Where a standard solution exists, use it and do not ask.

## Boundaries

These need the owner's decision before an agent builds them:

- New logic in the daemon beyond moving work through phases and keeping pods running, and any new control or constraint on agents. Expect a no.
- Programmatic checks on an agent's output (patterns run over what an agent wrote, checks on what a reviewer added).
- New requirements on the handoffs between roles.
- Deployment- or company-specific logic in Legion itself.

*Resist a change when:* it makes the daemon control agents, or guards against an agent doing its job.
