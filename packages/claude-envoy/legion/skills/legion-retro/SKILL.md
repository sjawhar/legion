---
name: legion-retro
description: Use when an issue has passed review and its parked implementer is revived for the mandatory pre-merge Legion retrospective.
---

# Legion Retro

Retro is mandatory for every issue that passed review. The daemon starts the implementer on it
once the reviewer approves, so the person with implementation context performs the retrospective,
and the skill obtains a separate fresh-eyes perspective. Retro runs before merge.

The `scripts/e2e/` proofs this skill names are in sjawhar/legion, the Legion repository, which
need not be the repository you are working in. `docs/solutions/` and `.legion/` are on the issue
branch of the repository you are working in.

## Merge-gate ordering

Follow this ordering exactly. It keeps the reviewed branch clean while preserving the
retrospective's durable output.

1. Tester green and all code-review cycles finish.
2. The reviewer approves the head. It still carries the issue's handoffs, `.legion/<issue>/`
   (`<issue>` is your `LEGION_ISSUE`), which every tester and reviewer round writes again. The
   architect then accepts the issue (`accepting`) with `sign_off`, which starts this retro; one it
   sends back to `implementing` or `testing` instead runs those phases again first.
3. Run this retro (*Durable outputs*, below): its learnings under `docs/solutions/`, then one final
   commit removing `.legion/<issue>/`, pushed together, and the retro message on the Dispatch
   issue. Retro writes **no handoff**. The removal exists because a squash merge commits the head
   merged into the base, and nothing on the base reads a handoff; the handoffs stay on the issue
   branch below that commit.
4. The merger quotes retro's commits (`jj diff --from <approved-sha> --to <tip-sha> --summary`) in
   READY, and sends the READY packet with its completion, which refuses a head with anything above
   the approved head but those commits, or still carrying `.legion/<issue>/`; the daemon
   posts it on the Dispatch issue and publishes it to the project's merge-queue role when one is
   configured. On READY's acceptance the daemon submits the merge, and it lands under the
   repository's rules: its required reviews and checks, and its merge queue when it enables one.
5. That merge closes the issue: the daemon writes `done`, and the implementer — not the reviewer or
   merger — is handed the production check in its live session, as work after the close: the agent
   that developed the change is responsible for testing it in production, once production carries
   the merge. The record is the pull request's `Production:` line and one pull-request comment,
   naming what was driven, how, what was observed, the merge commit and what woke the look, whether
   or not the issue is closed. A defect the check finds is a `dispatch ask` on the controller's
   report issue, never an issue the implementer files
   (`skill://legion-worker/references/merge-gate.md`, *The production check after the close*).

Retro's commits sit above the reviewer's approved head and the approval stands: commits that change
only `docs/solutions/`, and the one that removes `.legion/<issue>/`, do not void it, and the tree
goes from retro to the merger — never back to the tester or reviewer. A conflict merge pushed above
the approved head with no review round after it brings the base's paths above that head, so READY
refuses it (`READY_UNREVIEWED_CHANGE`) and the reviewer reviews the new head as a new round; one the
conflict procedure sends back through `implementing` is confirmed before READY. Any round after
retro (a conflict round, a round a red CI sends back, a re-review) writes `.legion/<issue>/` again,
and every approval, through the architect's acceptance, moves the issue to retro once more: each retro ends with the removal again, or
READY is refused.

Do not start retro before step 2 or skip it because the change seems mechanical; the merger's
`READY` comes only after step 3. The design gate is not a substitute for review and retro.

## Two perspectives

1. Re-read the issue, its acceptance criteria, the PR, test evidence, and review evidence.
   Confirm the PR carries both proofs: the implementer's own `E2E (implementer)` line and the tester's `E2E (tester)` line,
   each naming a production-like surface (the daemon's real-process tests and the live proofs under `scripts/e2e/`; a live check at the operator's next daemon restart, recorded on the PR; a sandbox repository, a devN
   stack, staging, or a local stack with real migrations), a command or run id, an observation, a head
   SHA, and a negative control. If either is missing, or links only a unit suite, the retro's first
   durable learning is that gap and the issue goes back — to the implementer for its own proof, to the
   tester for the tester's — before `READY`.
   Do not rebase or create a new branch; work on the existing issue branch.
2. Spawn one fresh-eyes subagent. Give it the issue and PR, ask it to inspect the diff and
   return concrete reusable learnings, and require it to return analysis rather than edit files.
3. Independently record the implementer's perspective: surprising constraints, difficult
   decisions, failed approaches, and reusable patterns.
4. Integrate the two perspectives. The implementer owns the final judgment: reject generic or
   context-free suggestions and preserve only learning that will help a future worker.

## Durable outputs

Before your turn's first commit, run `jj -R "$LEGION_WORKSPACE" log -r @ --no-graph -T 'empty'`.
When it prints `true`, run `jj -R "$LEGION_WORKSPACE" new @-`; otherwise `@` holds only your own
resumed edits, so run `jj -R "$LEGION_WORKSPACE" new`. Never `jj describe` or `jj split` the
working-copy commit you found at the turn's start; `skill://legion-worker` explains the
shared-workspace authorship rule.

Write the integrated learning as one or more discoverable documents under `docs/solutions/`.
Organize by reusable topic rather than by pull request, but never edit a document you did not
write this retro — not its frontmatter, not its body. Two trees' retros can land within the same
hour, and an in-place edit (another `related_issues` entry, a sharpened sentence, a `status`
flip) conflicts with any other tree's edit to the same lines of the same file. Search
`docs/solutions/` for the topic first; before relying on a hit, also search `docs/solutions/` for
its path (`docs/solutions/<its-path>`): a hit in another file's front matter under its
`supersedes:` key (on that line, or as an item of the list below it) supersedes the one you found,
and a line `Extends docs/solutions/<its-path>` extends it. Legion runs no pass that reconciles
these links, so a newer file is discoverable only by following them. Then write a new file of
your own, named `docs/solutions/<category>/<slug>-<LEGION_ISSUE>.md` so two trees never choose
the same path:

- **A fresh topic:** state the rule in a few imperative lines; the incident goes in an Evidence
  section below, never in the rule.
- **A topic an existing document already covers:** underneath its own H1 heading, the first line
  reads `Extends docs/solutions/<existing-path>.md.`; the rule states only the delta.
- **A topic an existing document states wrongly:** frontmatter adds
  `supersedes: docs/solutions/<existing-path>.md`, or, for several, a `supersedes:` list with one
  `- docs/solutions/<existing-path>.md` item each; the first line under the H1 says why. Leave
  the old file's frontmatter and body untouched: Legion runs no pass that reconciles it, so the
  `supersedes:` link is the only thing that makes the correction discoverable from the old file.

Each document uses this front matter:

```yaml
---
title: "Descriptive title matching the H1"
category: subdirectory-name
tags:
  - searchable-topic
date: YYYY-MM-DD
status: active
module: affected-module
related_issues:
  - "LEGION-123"      # the Dispatch issue
  - "owner/repo#456"  # the pull request
---
```

Commit the documentation on the existing issue branch. Do not create a replacement branch or
bookmark. Then commit the removal of the issue's handoffs as the one final commit, touching
nothing else, when the head still holds them:
`cd -- "$LEGION_WORKSPACE" && if [ -e ".legion/${LEGION_ISSUE:?}" ]; then rm -r -- ".legion/${LEGION_ISSUE:?}" && jj -R "$LEGION_WORKSPACE" split -m "retro: remove .legion/${LEGION_ISSUE:?}/ before READY" ".legion/${LEGION_ISSUE:?}"; fi`.
A retro that commits no learning makes that commit alone; a retro after an earlier removal with no
handoff written since has nothing to remove and makes none (without the guard, `rm -r` would fail
on the missing directory and that retro would stop there).

Before you push those commits, bring up to date the body content the repository derives from the
pull request's changed paths. A repository can require such content, a line naming a checklist
for each class of changed path for instance, and have a required check read it from the PR body.
When your commits add a `docs/solutions/` path the approved body never accounted for, or take away
the `.legion/` paths it did, that check fails at your head; the merger reports a stale body
rather than rewriting it, and GitHub will not merge a head whose required check failed, so the
merge waits.

1. Read what the deployment instructions in your system prompt, and the agent guide and pull
   request template of the repository you are working in, derive from the changed paths. When
   they derive nothing, skip steps 2 to 4 and go on to the push.
2. Compute it the way they say, for the paths the pull request changes at your head, the files
   GitHub lists on it:
   `base=$(gh pr view <n> -R <owner>/<repo> --json baseRefName -q '.baseRefName | @json') && cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch && jj -R "$LEGION_WORKSPACE" diff --from "fork_point(${base}@origin | <head>)" --to <head> --name-only`
   lists them, `base` the pull request's base branch as a jj string literal (never a name pasted between quotes) and `<head>` your removal commit. Where a
   line affirms work, such as a checklist completed for a class of path, do that work for the
   paths your commits add before you write the line: the line is a claim. That work may change
   files under `docs/solutions/` only, folded into your docs commit before the push, since
   anything else above the approved head voids the approval.
3. In a fresh `mktemp -d` directory outside `$LEGION_WORKSPACE` (the one place you write outside
   it: jj snapshots a file inside it, and a fixed name in a shared `/tmp` can hold another
   session's body), save the live body twice, and stop unless the read succeeded and is not
   empty. `pipefail` makes a failed read fail the line (without it the line exits with `tee`'s
   status and leaves two empty files), and the parentheses keep it to that line, since your
   session's shell persists and a later `cmd | head` would exit 141 under it:
   `cd -- "$LEGION_WORKSPACE" && ( set -o pipefail && gh api repos/{owner}/{repo}/pulls/{number} --jq .body | tee <dir>/before.md <dir>/body.md ) && test -s <dir>/before.md`.
   In `<dir>/body.md`, change the lines it carries for that content and add any it lacks where
   the instructions place them; other phases' lines stay as they wrote them.
4. Write it back before the push:
   `cd -- "$LEGION_WORKSPACE" && gh api --method PATCH repos/{owner}/{repo}/pulls/{number} -F body=@<dir>/body.md`.
   The checks the push starts read the body as it stands then; an edit after the push re-runs
   only a check that also starts on an edited body. Such a check re-runs now at the approved
   head, on a diff without your commits, and may fail there; READY reads the head your push makes.

When you cannot compute that content, cannot do the work a line affirms within
`docs/solutions/`, the body read fails or comes back empty, or GitHub refuses the body, push
nothing and do not complete: tell the architect with `envoy_publish` to its role topic, naming
what failed.

Then push both commits with one push, so one CI run covers them (`<KEY>` is your `LEGION_ISSUE`):
`cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" bookmark set legion/<KEY> -r @- && jj -R "$LEGION_WORKSPACE" git push --bookmark legion/<KEY>`.
Its head's message never ends with `skip-checks: true` and carries none of GitHub's
`[skip ci]`-family keywords (`skill://legion-worker`, *Every role pushes its own commits*).
Retro's push never carries the line, whatever paths it touches, because it is the last push before the merge and a base that requires a check will not merge a head whose checks never ran.
When the push is refused after step 4 wrote the body (`jj git push` refuses a branch another role
moved; never force), write `<dir>/before.md` back the same way before you report the refusal to
the architect with its output, so the body matches the head GitHub has.

When you have already pushed a head that wrongly carries the line — under an older copy of these
instructions, say — read that pushed head's own commit message first and confirm it ends with
`skip-checks: true`:
`cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" log -r <the pushed head> --no-graph -T description`.
The fix is to describe that same commit again without the line and push it once, which is the
rewrite of an already-pushed commit:
`skill://legion-worker/references/conflicts-and-rewrites.md#rewriting-pushed-commits`. Run that
section's guard first; a non-empty guard result means another tree's work is built on the commit,
so do not rewrite it — report the listed commits to the architect instead. Once the guard has
cleared, the bookmark moves backwards with `--allow-backwards` and the branch is pushed once:
that is the one non-fast-forward move the rewrite section sanctions, and not the force this
skill's push step forbids when `jj git push` refuses a branch another role moved. The rewritten
head is a code head, so GitHub runs the base branch's required checks on it from scratch, and the
merge waits for them.

After the push, post one Dispatch message on the issue — `--issue` is your `LEGION_ISSUE`; Legion
issues live on Dispatch, never on a GitHub
issue, so no `gh issue` write and no GitHub-issue comment — naming the documents, the
one-to-three most useful takeaways, the two proofs you read, and the production check that
follows the merge's close of the issue. The message must carry this revived implementer's structured
attribution footer with `phase` set to `retro`; the body is capped at 32,000 characters:

```bash
dispatch message --issue <KEY> --body-file - <<'EOF'
## Retro Complete

**Learnings documented in:**
- docs/solutions/<path>.md

**Key takeaways:**
- <reusable lesson>

**Proofs read:** implementer <surface/command>, tester <surface/command>.

**Production check:** <what the implementer will drive in production once the merge closes the issue and production carries it, and the deploy or restart that carries it there, with the agent or role the deployment instructions name to perform it, or the person it asks when they name none; no reply is awaited, the daemon wakes the check on the deploy's event>

<!-- legion: {"session":"<session-id>","phase":"retro"} -->
EOF
```

The `docs/solutions/` commit, the removal of `.legion/<issue>/`, the PR body lines those commits
make stale, and the Dispatch message are the only retro outputs. Never write a handoff, phase
artifact, local feedback log, or completion label, and change nothing under `.legion/` but that
removal. Report completion with the `legion` tool's `handoff_complete` alone (its summary: two
sentences for the architect); retro writes no handoff, so the commit it reports is the one your
workspace stands on, the removal commit your push made, so push before you complete.

## Completion check

Before returning, verify all of the following:

- The reviewer-approved head remains below the retro commits, and the reviewer's approval of that
  head stands: READY accepts the approved head plus the documentation commit and the removal.
- The learning documents and the Dispatch message both exist (never a GitHub issue comment).
- The PR body carries what the repository's instructions derive from the pull request's changed
  paths at the retro head, written back before that head's push, and each line that affirms work
  affirms work you did; or the instructions derive nothing.
- Both proofs were read, and any gap in either is recorded as a learning.
- Retro wrote no handoff, and when the head held `.legion/<issue>/`, its last commit, pushed,
  removed it and nothing else:
  `cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" file list -r <head> "root:.legion/${LEGION_ISSUE:?}"`
  prints nothing (`root:` names the path from the repository root, so a run from another directory
  cannot print nothing by failing).
- The fresh-eyes analysis was considered alongside the implementer's context.
- The merger remains a subsequent step, not work performed by retro.
