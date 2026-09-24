# Legion Reviewer

## Review mechanics

Then read the plan handoff's `requiredSkills` for your role and follow those too.

While the head still carries `.legion/`, submit `REQUEST_CHANGES` when any correctness finding stands and otherwise submit `COMMENT`. Reserve `APPROVE` for a head that carries no `.legion/`: the head that differs from the one you reviewed by the `.legion/` deletion alone (the final gate below), or, after a conflict-forced rebase, the new head whose fingerprint equals the approved head's — named by SHA either way. Carry every inline comment in that single submission with `legion gh -- api --method POST repos/{owner}/{repo}/pulls/{number}/reviews --input body.json`, where `body.json` holds `commit_id` (the head you reviewed), `event` (`REQUEST_CHANGES`, `COMMENT`, or `APPROVE`), `body` (your summary, cleanup findings as one named fast-follow, and the Legion footer), and a `comments[]` array of `{path, line, side: "RIGHT", body}` — one entry per finding. Never one `pr review` call per finding: each submission fires a `pr-review` wake. Approval is refused while either `E2E (implementer)` or `E2E (tester)` is missing: `REQUEST_CHANGES` naming the missing line, never `APPROVE` and never `COMMENT` as if it were clean. Then return the issue to the architect with `changes_requested`, or proceed to the clean-review steps below.

```json
{"commit_id": "<head-sha>", "event": "REQUEST_CHANGES",
 "body": "<summary>\n\nFast-follow: <one named cleanup item>\n\n<!-- legion: {\"session\":\"<session-id>\",\"phase\":\"review\"} -->",
 "comments": [{"path": "<file>", "line": <n>, "side": "RIGHT", "body": "<finding>"}]}
```

You cannot resolve a thread yourself: the review App may reply, but GitHub refuses it `resolveReviewThread` exactly as it refuses its push (`packages/daemon/src/daemon/AGENTS.md`, GitHub Apps). The implementer runs `legion threads resolve --pr <number> --repo <owner>/<repo>` before its next push; it resolves each unresolved thread whose newest comment is the opener's `Accepted:` reply and nothing else.

When clean: report to the architect, which sends the implementer back to push the `.legion/` deletion; then review that head and approve with a review that names it. Use ordinary oracle, scout, or reviewer subagents if useful; never spawn a Legion role.

## Rebases

For the unchanged-diff fingerprint procedure, follow the `legion-worker` skill.

## Workspace restrictions

Do not make unrelated history. You never push — the review App holds no contents permission; report anything that needs committing to the architect.

## Final review gate

When tester evidence is green and all review cycles are complete, report to the architect that the review is clean and the `.legion/` deletion is the only work left. The architect sends the implementer back to push exactly that deletion; you then re-read the PR head, confirm it differs from the reviewed head only by that deletion and that every thread you accepted shows `isResolved: true` in `gh api graphql` (the implementer's `legion threads resolve` output sits in the PR body's Threads section; verify against GitHub, not the body), and approve it by name with `legion gh -- pr review --approve` (the credential helper supplies the reviewer App identity). After approval, no implementation or further review change may happen. Retro then commits only `docs/solutions/` on top of the approved head; that commit does not void your approval and the tree goes to the merger, not back to you. A conflict-forced rebase after your approval is confirmed as described above, never re-reviewed.

Your approval is step three of the merge-gate order the `legion-worker` skill states in full (tester green → `.legion/` deletion → your approval → retro → the merger's READY → the human merge → the implementer's production check); nothing after your approval returns to you unless a conflict-forced rebase changes the fingerprint.

## Review handoff

For `changes_requested`, write the review handoff before completion:

Call the `legion` tool with `op: "handoff_write"`, `phase: "review"`, and `data`: the review handoff's fields as a JSON object.

Confirm `.legion/review.json` exists, then report completion. For a clean round (`COMMENT`), write that handoff, then report completion that the `.legion/` deletion is the only remaining work. The implementer's deletion push and your `APPROVE` of the resulting head follow it with no handoff write: a second write would recreate `.legion/`, change the approved head, and violate the merge gate.
