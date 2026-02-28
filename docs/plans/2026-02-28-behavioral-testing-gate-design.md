# Behavioral Testing Gate

## Goal

Add a mandatory behavioral testing phase to the Legion worker pipeline. A fresh agent boots the application, exercises user-facing behavior against running infrastructure, and verifies acceptance criteria before code review begins. This catches the class of bugs that unit tests miss — code that passes `bun test` but doesn't actually work when a user interacts with it.

## Motivation

From Legion Retro (#63): "Unit tests are the weakest form of verification. Workers write code that passes `bun test` but we have no idea if the Bitwarden context menus actually work, or if LinkedIn search actually returns fuzzy results."

The current quality gate (run `bun test`, `tsc`, `biome` before dispatching reviewer) is necessary but insufficient. It verifies the code compiles and unit tests pass, but never verifies that the feature works end-to-end. A fresh agent with no implementation context provides independent behavioral verification — the same way a real user would discover bugs the developer missed.

## Decisions

- **New worker mode** — `test` joins the existing 5 modes (architect, plan, implement, review, merge)
- **New issue status** — `Testing` sits between `In Progress` and `Needs Review`
- **Mandatory gate** — every issue passes through Testing; this is not opt-in
- **Fresh agent** — the tester is a new session, not the implementer; independence prevents confirmation bias
- **Controller owns transitions** — the controller moves issues between statuses; workers signal completion via `worker-done` label
- **Label-based pass/fail** — tester adds `test-passed` or `test-failed` label; controller reads these to decide next action
- **Failure loops back** — test failure returns the issue to In Progress and resumes the same implementer session with the failure report
- **Upstream workflows feed the tester** — architect flags infra gaps, planner writes the testing plan, implementer updates docs

## Pipeline

### Updated lifecycle

```
architect → plan → implement → test → review → retro → merge
```

### Updated issue statuses

```
Backlog → Todo → In Progress → Testing → Needs Review → Retro → Done
```

### Phase responsibilities

| Phase | Responsibility | Testing-related change |
|-------|---------------|----------------------|
| Architect | Break down issues, define acceptance criteria | Add testing infrastructure assessment: flag gaps (no local server setup, missing seed data, no browser test harness) |
| Planner | Create implementation plan | Add testing plan section: boot commands, health checks, verification steps per criterion, tools needed |
| Implementer | TDD implementation, PR creation | Must update docs for user-facing changes; must add `worker-done` on exit (currently exits without it) |
| Tester | **New.** Behavioral verification | Boot app, execute testing plan, capture evidence, post results + doc feedback to PR |
| Reviewer | Code review | No change — but now receives behaviorally-verified PRs with tester comments for context |
| Merger | Merge PR | No change |

## Tester Workflow

### Inputs

- The issue (with acceptance criteria from architect)
- The testing plan (from planner, in issue comments or plan doc)
- The PR (code changes, implementer's documentation)

### Steps

1. **Setup** — Fetch issue, testing plan, and PR metadata. Check out the branch.

2. **Read the docs first** — Before doing anything else, try to understand the feature from the repo's documentation alone. The tester's first experience with the docs mirrors a real user's experience. This is intentional.

3. **Boot the environment** — Follow the testing plan's setup instructions (start servers, seed data, build assets, etc.). If the environment fails to boot, that's a test failure.

4. **Execute acceptance criteria** — Work through each criterion from the testing plan using appropriate tools:
   - Playwright / agent-browser for web UIs
   - curl / HTTP requests for APIs
   - CLI commands for command-line tools
   - Subprocess execution for scripts and build tools

5. **Capture evidence** — For each criterion: screenshots, log output, command output, or other concrete artifacts. Not just "it worked" — actual proof.

6. **Post results to PR** — A structured comment containing:
   - Pass/fail for each acceptance criterion with evidence
   - Documentation quality feedback (was it easy to get started? anything missing or confusing?)
   - Observations about UX, error messages, or edge cases noticed during testing

7. **Signal completion** — Add `worker-done` label plus `test-passed` or `test-failed` label.

### Failure path

Tester fails → adds `worker-done` + `test-failed` → controller transitions issue back to In Progress → controller resumes the same implementer session with the test failure report → implementer fixes → adds `worker-done` → controller transitions to Testing → dispatches fresh tester.

## State Machine Changes

### New types

```
IssueStatusLiteral: add "Testing" (between "In Progress" and "Needs Review")

WorkerModeLiteral: add "test" (6 modes: architect, plan, implement, test, review, merge)

ActionType: add 3 new actions:
  - "transition_to_testing"
  - "dispatch_tester"
  - "resume_implementer_for_test_failure"
```

### Decision logic

```
In Progress + worker-done               → transition_to_testing  (was: transition_to_needs_review)

Testing + no worker-done + no live worker           → dispatch_tester
Testing + worker-done + test-passed label           → transition_to_needs_review
Testing + worker-done + !test-passed label      → resume_implementer_for_test_failure
Testing + live worker                               → skip
```

**Note:** The state machine checks `hasTestPassed` (presence of `test-passed`), not `hasTestFailed`. The `test-failed` label is for human visibility and controller cleanup only. `hasPr` is not checked for `transition_to_testing` — the controller's quality gate catches missing PRs before dispatching the tester.

### Label lifecycle

The controller handles label cleanup during transitions:
- On `transition_to_testing`: remove `worker-done` from In Progress
- On `transition_to_needs_review` from Testing: remove `worker-done`, `test-passed`
- On `resume_implementer_for_test_failure`: remove `worker-done`, `test-failed`, transition to In Progress

**Post-design additions (implemented during development):**
- Spec compliance check added as step 2 of tester workflow (before reading docs)
- Review-changes → testing loop: reviewer requests changes → controller transitions to In Progress → implementer fixes → testing gate runs again
- Controller must transition to In Progress before resuming implementer for changes (avoids infinite loop)

### Implementer `worker-done` change

Currently the implementer opens a draft PR and exits without adding `worker-done`. The state machine detects completion implicitly (Needs Review status + no worker + no worker-done). With the test gate, the implementer should add `worker-done` explicitly so the state machine has a clear signal to transition to Testing. This is cleaner than the current implicit detection.

## Upstream Workflow Modifications

### Architect workflow

Add a **Testing Infrastructure Assessment** after acceptance criteria:

- Can the acceptance criteria be verified against running infrastructure?
- What infrastructure is needed? (local server, browser, database, seed data, etc.)
- What's missing? (no docker-compose, no seed script, README doesn't explain how to run locally)

This surfaces during the user's review of the architect output, before planning begins. If major gaps exist, the user can address them or scope them into the issue.

### Planner workflow

Add a **Testing Plan** section to the plan output:

- **Setup steps** — concrete commands to boot the environment
- **Health check** — how to verify the environment is ready (URL, port, expected response)
- **Verification steps** — for each acceptance criterion: specific actions and expected outcomes
- **Tools needed** — what the tester should use (Playwright, curl, CLI, etc.)

### Implementer workflow

Two additions to exit criteria:

1. **Documentation requirement** — for any user-facing behavior change, update relevant docs (README, usage guides, API docs) before creating the PR. Docs explain how to use the feature, not just what changed.
2. **Explicit `worker-done`** — add `worker-done` label on exit instead of relying on implicit detection.

## What This Doesn't Cover

- **Testing infrastructure provisioning** — the tester uses whatever exists. If a repo doesn't have a way to run locally, the architect flags this and the user addresses it. Legion doesn't create infrastructure.
- **Performance testing** — the tester verifies behavior, not performance. Performance benchmarking is a separate concern.
- **Flaky test handling** — the tester gets one shot. If the test fails, it's assumed to be a real bug, not flakiness. As repos mature with Legion, testing infrastructure stabilizes and false negatives become rare.
- **CI status integration** — tracked separately in #62. The tester is behavioral verification; CI is automated checks. They complement each other.

## Related Issues

- #63 — Legion Retro (motivation, "your tester idea — strong agree")
- #25 — E2E Testing Capability Set (closed; established E2E as loadable capability, superseded by this design making it mandatory)
- #62 — State machine CI status (complementary; CI checks are orthogonal to behavioral testing)
- #30 — Pre-transition quality gates (existing gate; behavioral testing is the next layer)
