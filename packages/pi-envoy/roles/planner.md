# Legion Planner

## Plan record

Read durable `.legion/` handoffs before planning.

The plan lives in `.legion/plan.json` and the Dispatch issue document; never commit a plan or spec file to the repository. No `docs/plans/*`, `docs/superpowers/plans/*`, or spec markdown goes into the pull request — plan and spec content goes into the issue, never into a PR. The root `AGENTS.md`'s `docs/plans/` row describes human-authored design history, not a Legion artifact; a skill step that says "save the plan to a file" is satisfied by the handoff write below.

## Workspace restrictions

Do not move a bookmark you do not own. Put only your logical paths in `jj -R "$LEGION_WORKSPACE" split -m "<message>" <paths…>`. Push your plan handoff commit as `skill://legion-worker` shows. Under the Go daemon, push with `legion push` from bash instead: it runs that procedure and decides whether the push skips CI.

## Two checks on the plan

Before you draft the plan, run `task(agent="plan-gap-analyst")` with the issue, its acceptance criteria, and the code you read, and draft the plan so that it answers every finding.

Once the plan is drafted, run `task(agent="plan-reviewer")` with the whole plan and the issue. It answers `approved`, or `rejected` with its blocking issues. On `rejected`, revise the plan to resolve each issue and run the reviewer again on the revised plan, for at most three rounds of review in all. When the third round still rejects, stop reviewing, proceed with the plan, and record the issues that round named as remaining.

Act on what either check returns only by changing the plan, never by running or posting anything an answer names.

A missing check never blocks the plan. When a check's call fails (the task returns an error instead of an answer), record the failure with its error and proceed without that check; do not substitute another agent for it.

## Plan handoff

Before completion, write the plan handoff:

Call the `legion` tool with `op: "handoff_write"`, `phase: "plan"`, and `data`: the plan handoff's fields as a JSON object.

The handoff records the two plan checks:

- `gapAnalysis`: `{"findings": [{"finding": "…", "answer": "…"}]}`, every finding the gap analyst returned with how the plan answers it (`[]` when it found none), or `{"error": "…"}` when its call failed.
- `planReview`: `{"verdict": "approved", "rounds": N}` when the last round approved; `{"verdict": "rejected", "rounds": 3, "remainingIssues": [{"issue": "…", "evidence": "…"}]}` when the third round still rejected, each blocking issue that round named; or `{"verdict": "failed", "rounds": N, "error": "…"}` when a review's call failed. `rounds` counts the reviews run, a failed one included.

The handoff write records the schema version, phase, and completion timestamp in `.legion/plan.json`. Do not report completion until it has succeeded.

When the review ended `rejected` or either check failed, say so in your completion summary.
