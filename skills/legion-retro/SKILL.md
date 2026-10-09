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
   (`<issue>` is your `LEGION_ISSUE`), which every tester and reviewer round writes again.
3. Run this retro (*Durable outputs*, below): its learnings under `docs/solutions/`, then one final
   commit removing `.legion/<issue>/`, pushed together, and the retro message on the Dispatch
   issue. Retro writes **no handoff**. The removal exists because a squash merge commits the head
   merged into the base, and nothing on the base reads a handoff; the handoffs stay on the issue
   branch below that commit.
4. The merger verifies the tip is the approved head plus retro's commits, quotes their
   `jj diff --from <approved-sha> --to <tip-sha> --summary` in READY, and sends the READY packet
   with its completion, which refuses a head that still carries `.legion/<issue>/`; the daemon
   posts it on the Dispatch issue and publishes it to the project's merge-queue role when one is
   configured. A human merges under the repository's GitHub branch-protection and CODEOWNERS
   requirements; GitHub's merge queue participates only when the repository enables it.
5. After that merge, the implementer — not the reviewer or merger — verifies the change in production
   and records it on the PR and the issue: the agent that developed it is responsible for testing
   in production. The architect's sign-off waits for that record.
   The record is the pull request's `Production:` line, one pull-request comment, and a
   `dispatch message` on the issue, each naming what was driven, how, what was observed, and the
   merge commit. A defect the production check finds becomes a corrective child issue of the same tree,
   owned by the architect and implemented by the same implementer; the parent stays open until it lands.

Retro's commits sit above the reviewer's approved head and the approval stands: commits that
change only `docs/solutions/`, and the one that removes `.legion/<issue>/`, do not void it, and the
tree goes from retro to the merger — never back to the tester or reviewer. A conflict-forced
rebase after retro moves these commits with the branch. Any round after retro (a conflict round, a
round a red CI sends back, a re-review) writes `.legion/<issue>/` again, and every approval moves
the issue to retro once more: each retro ends with the removal again, or READY is refused.

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

Write the integrated learning as one or more discoverable documents under `docs/solutions/`.
Organize by reusable topic rather than by pull request, but never edit a document you did not
write this retro — not its frontmatter, not its body. Two trees' retros can land within the same
hour, and an in-place edit (another `related_issues` entry, a sharpened sentence, a `status`
flip) conflicts with any other tree's edit to the same lines of the same file. Search
`docs/solutions/` for the topic first; before relying on a hit, also search for
`supersedes: docs/solutions/<its-path>` and `Extends docs/solutions/<its-path>` naming it —
Legion runs no pass that reconciles these links, so a newer file that extends or supersedes the
one you found is discoverable only by following them. Then write a new file of your own, named
`docs/solutions/<category>/<slug>-<LEGION_ISSUE>.md` so two trees never choose the same path:

- **A fresh topic:** state the rule in a few imperative lines; the incident goes in an Evidence
  section below, never in the rule.
- **A topic an existing document already covers:** underneath its own H1 heading, the first line
  reads `Extends docs/solutions/<existing-path>.md.`; the rule states only the delta.
- **A topic an existing document states wrongly:** frontmatter adds
  `supersedes: docs/solutions/<existing-path>.md`; the first line under the H1 says why. Leave
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
the `.legion/` paths it did, that check fails at your head; the merger reports a stale body rather
than rewriting it, and READY refuses a head whose required check failed, so the tree stops at the
merger.

1. Read what the deployment instructions in your system prompt, and the agent guide and pull
   request template of the repository you are working in, derive from the changed paths. When
   they derive nothing, skip steps 2 to 4 and go on to the push.
2. Compute it the way they say, for the paths the pull request changes at your head, the files
   GitHub lists on it:
   `cd -- "$LEGION_WORKSPACE" && jj -R "$LEGION_WORKSPACE" git fetch && jj -R "$LEGION_WORKSPACE" diff --from 'fork_point(<base>@origin | <head>)' --to <head> --name-only`
   lists them, `<base>` the pull request's base branch and `<head>` your removal commit. Where a
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
   `cd -- "$LEGION_WORKSPACE" && ( set -o pipefail && legion gh -- api repos/{owner}/{repo}/pulls/{number} --jq .body | tee <dir>/before.md <dir>/body.md ) && test -s <dir>/before.md`.
   In `<dir>/body.md`, change the lines it carries for that content and add any it lacks where
   the instructions place them; other phases' lines stay as they wrote them.
4. Write it back before the push:
   `cd -- "$LEGION_WORKSPACE" && legion gh -- api --method PATCH repos/{owner}/{repo}/pulls/{number} -F body=@<dir>/body.md`.
   The checks the push starts read the body as it stands then; an edit after the push re-runs
   only a check that also starts on an edited body. Such a check re-runs now at the approved
   head, on a diff without your commits, and may fail there; READY reads the head your push makes.

When you cannot compute that content, cannot do the work a line affirms within
`docs/solutions/`, the body read fails or comes back empty, or GitHub refuses the body, push
nothing and do not complete: tell the architect with `envoy_publish` to its role topic, naming
what failed.

Then push both commits with one `legion push`, so one CI run covers them. When the push is
refused after step 4 wrote the body, write `<dir>/before.md` back the same way before you report
the refusal to the architect, so the body matches the head GitHub has. After the push, post one
Dispatch message on
the issue — `--issue` is your `LEGION_ISSUE`; Legion issues live on Dispatch, never on a GitHub
issue, and the `gh` shim refuses every GitHub-issue write — naming the documents, the
one-to-three most useful takeaways, the two proofs you read, and the production check that
follows the merge. The message must carry this revived implementer's structured
attribution footer with `phase` set to `retro`; the body is capped at 2,000 characters:

```bash
dispatch message --issue <KEY> --body-file - <<'EOF'
## Retro Complete

**Learnings documented in:**
- docs/solutions/<path>.md

**Key takeaways:**
- <reusable lesson>

**Proofs read:** implementer <surface/command>, tester <surface/command>.

**Production check:** <what the implementer will drive after the merge, or the deploy/restart step a human will have to perform first>

<!-- legion: {"session":"<session-id>","phase":"retro"} -->
EOF
```

The `docs/solutions/` commit, the removal of `.legion/<issue>/`, the PR body lines those commits
make stale, and the Dispatch message are the only retro outputs. Never write a handoff, phase
artifact, local feedback log, or completion label, and change nothing under `.legion/` but that
removal. Report completion with the `legion` tool's `handoff_complete` alone (its summary: two
sentences for the architect) — no `handoff_write`.

## Completion check

Before returning, verify all of the following:

- The reviewer-approved head remains below the retro commits, and the reviewer's approval of that
  head stands: the merger accepts the approved head plus the documentation commit and the removal.
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
