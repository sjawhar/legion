# The `skip-checks: true` trailer, and what starts a workflow run

Part of `skill://legion-worker`. Read it before you write the trailer on a handoff-only push, and
whenever you expect a pull-request body edit to re-run a check. `skill://legion-worker/SKILL.md`
carries the rule — which push carries the trailer, which does not, and that a wrong trailer stalls
the issue; this is its shape, its command, and its consequences. Every path it cites is in
sjawhar/legion.

## The shape GitHub honours

A push whose commits touch only `.legion/` (a handoff-only push) ends its head's commit message
with one trailer block whose last line is `skip-checks: true`, preceded by two empty lines, so
GitHub starts no workflow run for it and the daemon carries the code head's verdict to it — except
retro's push, which never carries the line whatever paths it touches, for the reason the next
section gives. GitHub's rule ("Skipping workflow runs",
https://docs.github.com/en/actions/how-tos/manage-workflow-runs/skip-workflow-runs): "The trailers
section should be included at the end of your commit message and be preceded by two empty lines.
If you already have other trailers in your commit message, `skip-checks` should be last." One empty
line is not enough, and GitHub then starts every workflow as if the line were not there (a
planner's handoff push did exactly that in the first live run of this rule). The `Omp-Session:`
trailer the pane's jj overlay wrote stays in the same block, directly above `skip-checks: true`;
set below the two empty lines on its own, it would no longer be in the message's last paragraph
and git's trailer parser would stop seeing it. jj keeps the two empty lines as written, where
`git commit`'s default cleanup would collapse them. The describe splits the existing trailer
paragraph off the message (`git interpret-trailers --parse` finds it), writes the body, two empty
lines, those trailers and `skip-checks: true`, with trailers off for the one describe, since the
overlay would otherwise append a second `Omp-Session:` after it:

```bash
d=$(jj -R "$LEGION_WORKSPACE" log -r @- --no-graph -T description) && t=$(printf '%s\n' "$d" | git interpret-trailers --parse) && n=$'\n' && { [ -z "$t" ] || d=${d%$n$n*}; } && jj -R "$LEGION_WORKSPACE" describe -r @- --config 'templates.commit_trailers=""' -m "$d$n$n$n${t:+$t$n}skip-checks: true"
```

## Every other push carries no such line

Every other push — code, red tests, `docs/solutions/`, a forward merge, a rewrite — carries no
such line and none of GitHub's bracket keywords (`[skip ci]`, `[ci skip]`, `[no ci]`,
`[skip actions]`, `[actions skip]`), which start no workflow wherever they stand.
Retro's push never carries the line, whatever paths it touches, because it is the last push before the merge and a base that requires a check will not merge a head whose checks never ran.
That is sjawhar/legion#2143's sentence, and `packages/pi-legion/src/skills-guard.test.ts` counts
it here and in the four other texts that state it, so none of the five can drift from the rest.
The daemon classifies a push by the paths its commits touch, never by the
trailer: a wrong trailer stalls the issue (a trailered code push gets no verdict carried, and a
head with no CI is one a base that requires a check will not merge, so the merge waits and the
daemon tells the issue's architect once it has waited an hour) and never passes it.

## When a body edit re-triggers a workflow, and when it does not

While the head commit carries `skip-checks: true`, a pull-request body edit alone starts no GitHub
workflow run, so a check that re-judges the body re-runs on that head only after a later push; on
a code head the same edit re-runs it at once. While the pull request conflicts with its base
(GitHub shows it `CONFLICTING`), no workflow triggered `on: pull_request` runs for any of its
activity types, so a body edit re-judges nothing there either; a push cures both, but only once
the pull request is mergeable, so the implementer forward-merges a conflicting one first
(*Reintegrating the base* in `skill://legion-worker/references/conflicts-and-rewrites.md`).
