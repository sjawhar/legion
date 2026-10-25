# Testing a slice with a browser page

Part of `skill://legion-worker`. Read it when you are the tester and the slice changes a page a
person reads in a browser. It adapts `skill://ce-dogfood` to the tester's contract, and where the
two differ this reference wins.

## When

Run `skill://ce-dogfood` in your testing round, after you have verified the implementer's proof,
in addition to your other checks. A slice whose diff changes no such page gets no dogfood run.

## Target

Dogfood the issue's own checkout in `$LEGION_WORKSPACE`. Create no worktree, use no other
checkout, and skip ce-dogfood's isolation step and `ce-worktree`. The diff scope is the issue
branch against its fork point from the issue's default branch, with `$default` read as
`skill://legion-worker/references/default-branch-check.md` says:

```bash
jj -R "$LEGION_WORKSPACE" diff --from "fork_point(${default}@origin | @-)" --to @-
```

Never diff from `${default}@origin` itself: once the default branch has moved past the fork point,
that diff shows the default branch's newer commits as deletions. In a git worktree of the clone,
`git diff origin/<default>...HEAD` gives the same diff.

## Data and persona

Serve the page with the data a person sees: the repository's seeded local stack or harness, or
another production-like stack. Walk the flows as the person the issue is for, named from its spec.

## What replaces the fix loop

Fix nothing: ce-dogfood's Phase 5 is not yours. Your only commits are your red tests and your
handoff, as in any testing round. Each failing scenario, contradicted criterion or sharp paper cut
becomes a `failures` entry in `.legion/<issue>/test.json`, pinned by a red test in the repository's
own suite. Use its browser suite where it has one (Legion's own is `packages/dispatch/e2e/`).
Where no automated test can assert a defect, such as a pure copy or spacing defect, its entry
carries the `agent-browser` commands that replay it and a screenshot path, and says why no test is
meaningful. Complete the phase with `verdict: "fail"`, as for any defect; the resumed implementer
fixes it. Your next testing round replays each failed scenario in the browser.

## What replaces the committed report

Keep the checkpoint and the final report under `$TMPDIR`, never in the repository: write no
`dogfood-reports/` file and commit no report (ce-dogfood's Phase 6). Post the final report on the
slice's Dispatch issue as an artifact, a new version each round:

```bash
dispatch artifact --issue <KEY> --name dogfood-report.md --content-file "$TMPDIR/<report>.md"
```

Your `E2E (tester)` line links it as `dispatch://<KEY>/artifact/dogfood-report-md`. Screenshots
stay under `$TMPDIR` unless the report embeds one.

## Decisions for a person

Send each entry of the report's "Decisions for a Human", each `Blocked (needs human verify)` leg
and each stale pack rule to the tree's architect, with `envoy_publish` to its role topic. The
architect writes any decision block. Never ask the person directly.

## The suite

ce-dogfood's "run the project's automated test suite once" is CI's work here. Read the head's CI
on GitHub as `skill://legion-worker/references/pr-body.md` says, and rerun nothing.

## Helpers Legion does not ship

`ce-worktree`, `ce-debug`, `ce-commit`, `ce-compound` and `ce-setup` are not installed: do the step
yourself or leave it out. List what ce-dogfood would hand to `ce-compound` under "Pack candidates".

## Caches

Set `CE_PACKS_CACHE_ROOT="$TMPDIR/ce-packs"` before you run ce-dogfood's `scripts/packs-resolve.py`;
without it the script tries `/tmp` first. Keep screenshots under `$TMPDIR` too.

## Browser

`agent-browser` is on `PATH` in the worker image, from the release that ships this reference on.
It launches the image's `chromium` with no extra flags (measured under gVisor, as uid 1000).
