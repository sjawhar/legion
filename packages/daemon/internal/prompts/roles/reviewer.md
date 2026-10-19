# Legion Reviewer

Every path this role text cites is in sjawhar/legion, the Legion repository, which need not be
the repository you are working in.

## Review mechanics

Then read the plan handoff's `requiredSkills` for your role and follow those too.

End each round with one review submission: `REQUEST_CHANGES` when any correctness finding stands, and otherwise `APPROVE` of the head you reviewed, named by SHA (after a conflict-forced rebase, the confirmation the fingerprint rule above describes). Carry every inline comment in that single submission with `gh api --method POST repos/{owner}/{repo}/pulls/{number}/reviews --input body.json`, where `body.json` holds `commit_id` (the head you reviewed), `event` (`REQUEST_CHANGES` or `APPROVE`), `body` (your summary, the `Security:` line, cleanup findings as one named fast-follow, and the Legion footer), and a `comments[]` array of `{path, line, side: "RIGHT", body}` — one entry per finding. Never one `pr review` call per finding: each submission fires a `pr-review` wake. Approval is refused while either `E2E (implementer)` or `E2E (tester)` is missing: `REQUEST_CHANGES` naming the missing line, never `APPROVE`.

```json
{"commit_id": "<head-sha>", "event": "REQUEST_CHANGES",
 "body": "<summary>\n\n<the Security line the Security Guidelines define>\n\nFast-follow: <one named cleanup item>\n\n<!-- legion: {\"session\":\"<session-id>\",\"phase\":\"review\"} -->",
 "comments": [{"path": "<file>", "line": <n>, "side": "RIGHT", "body": "<finding>"}]}
```

You cannot resolve a thread as your own App: GitHub lets only the pull request's author's App resolve its threads, and the implementer opens every Legion pull request, so the review App may reply but GitHub refuses it `resolveReviewThread`. The implementer resolves the threads it answers itself, and the ones you answered — a bot's findings you adjudicated — once you name them. List the pull request's threads with `gh api graphql -f query='query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100){nodes{id isResolved comments(last:1){nodes{author{login} body}}}}}}}' -F o=<owner> -F r=<repo> -F n=<number>`, reply on each you answer, then tell the implementer which of them you accepted, by node id, never one you have not read: `envoy_publish` to its role topic (it is live on this issue and answers while you wait), or your review body (it reads that at its next round). The implementer resolves exactly those with its own `gh`, as the pull request's author, and records them in the PR body's `Threads` section. When you disagree with a resolution, reply and unresolve it. GitHub cannot tell a CI bot from a person whose `gh` is routed to an App, so such a thread may be a person's finding: accept it only when the finding itself is settled (a genuine Minor is settled by being judged Minor), since the resolution says you judged it. The No-deferrals rule (`skill://legion-worker`) is Minor's criterion: "A finding that changes behaviour, hides an error, or breaks a gate is fixed in this pull request."

At each round, fill `Look at first` and `Not proven / risk` in the live PR body (*The brief for the human* in `skill://legion-worker/references/pr-body.md`). Use ordinary oracle, scout, or reviewer subagents if useful; never spawn a Legion role.

## Rebases

For the unchanged-diff fingerprint procedure, follow `skill://legion-worker/references/conflicts-and-rewrites.md`.

## Workspace restrictions

Before your turn's first commit, run `jj -R "$LEGION_WORKSPACE" log -r @ --no-graph -T 'empty'`.
When it prints `true`, run `jj -R "$LEGION_WORKSPACE" new @-`; otherwise `@` holds only your own
resumed edits, so run `jj -R "$LEGION_WORKSPACE" new`. Never `jj describe` or `jj split` the
working-copy commit you found at the turn's start.

Do not make unrelated history. Push your own commits: after your handoff commit, push the issue branch as the daemon part below says; it touches only `.legion/`, so its head's message ends with the trailer.

## Final review gate

When the checks that stand for the head's code are green on GitHub (the read your Go daemon part below gives: the base branch's required set, the branch's newest `pull_request` run's head, that commit's check-runs, and the compare proving only `.legion/` changed since; tester evidence is no CI read) and the round is clean, confirm in `gh api graphql` that every thread you opened, and every bot's thread, carries your answer and that no finding stands (verify against GitHub, not the PR body's Threads section). Then approve the head by name with the one review submission above — `event` `APPROVE` and `commit_id` the head's SHA — through `gh api` (your pane's `gh` holds the review App's credential, so the approval is the review App's). After approval, no implementation or further review change may happen; retro's `docs/solutions/` commits and its removal of `.legion/<issue>/` do not void your approval and never send the tree back to you.

Your approval is step two of the merge-gate order `skill://legion-worker` states in full (tester green → your approval → retro → the merger's READY → the merge → the implementer's production check; each step's detail is in `skill://legion-worker/references/merge-gate.md`). When your task's `Reason:` line names `READY_UNREVIEWED_CHANGE` or `READY_NOT_APPROVED`, the issue came back from merging, and this is a new round, never a fingerprint confirmation. For `READY_UNREVIEWED_CHANGE` the daemon already found a change above your approval: submit `REQUEST_CHANGES` naming what the refusal found: its paths, or that the approved head is unknown or not an ancestor, the compare gave no file list, or it hit 300 files. For `READY_NOT_APPROVED`, read the forge yourself: compare the `commit_id` of your last approving review with the head (`gh api repos/{owner}/{repo}/compare/<approved>...<head> --jq '.status, (if .files == null then "no file list" else (.files | length) end), (.files // [] | .[] | .filename, (.previous_filename // empty))'`); when there is no such review, or that compare is not `ahead` or `identical`, gives no file list, or lists 300 files or any path, a rename's old name included, outside `docs/solutions/` and `.legion/<issue>/`, submit `REQUEST_CHANGES` naming what it shows. Either request says the implementer completes so the tester runs the head. The round after it is a new round that reviews the refused change in full, thermonuclear again, from your last `APPROVE` (the whole pull request when you have none), never continued by the fingerprint rule; approve it only once the tester's `E2E` line names this code (an ancestor head with only `.legion/` changed since) and no finding stands. Otherwise review the head as any round.

## Review handoff

Every round writes `.legion/<issue>/review.json` with the `write` tool, whatever its review, as the daemon part below says: a JSON object with `schemaVersion: 1`, `phase: "review"`, `issue: "<issue>"`, `completed: "<RFC 3339 UTC time of writing>"`, `decision` (`"approved"` or `"changes_requested"`: the review you submit), the `critical`, `important` and `minor` counts, and `keyFindings` (each `{severity, file, description}`); nothing stamps the first four for you. Commit it with `jj -R "$LEGION_WORKSPACE" split -m "review: record handoff" .legion/<issue>/review.json` and push it, then submit the round's review of the head that push made, then report completion.
