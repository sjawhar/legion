# Review threads

Part of `skill://legion-worker`. Read it when you reply to, accept, or resolve a review thread,
or run `legion threads resolve`: the implementer after every push that answers a review, the
merger before READY, and the reviewer, who answers threads on every re-review and runs the command
only when a review workflow the project declares is red on a bot's findings.
Every path it cites is in sjawhar/legion.

- **Threads are dispositioned individually, never resolved in bulk.** Every open review
  thread gets its own line naming the fixing commit or the reason it isn't a defect. The
  reviewer answers each thread it opened, and each thread a bot opened that is none of Legion's
  role Apps, with exactly one of `Accepted: fixed in <commit> — <one line>`,
  `Accepted: not a defect — <reason>`, or `Still open: <what remains>`; nothing else is an
  acceptance, and nobody replies after an `Accepted:` (any later reply that is not itself an
  `Accepted:` — the opener's own follow-up included — leaves the thread open, because resolution
  considers only the newest comment). The review App can reply on a thread but cannot resolve it:
  GitHub grants resolving a review thread to the pull request's author, and the implementer opens
  every Legion pull request (`docs/site/src/content/docs/legion/running-legion.md`, "The two
  GitHub Apps"). In the reviewer's pane (`LEGION_ROLE=reviewer`), `legion threads resolve` asks
  the daemon instead (`POST /legion/v1/threads/resolve`), which resolves as the implementer's App
  only the threads a bot outside Legion's role Apps opened whose newest submitted comment is the
  reviewer's `Accepted:`, on the pull request of the reviewer's own issue, and prints the same
  lines; the reviewer never holds the implementer's token. A thread whose newest comment is a
  draft in the implement App's pending review (the implementer's or the merger's: GitHub shows it
  to that App alone, as which the daemon reads) is left open and never named: the command prints
  only how many there are (`<n> unresolved threads hold the implement App's pending draft and
  were left open`) and exits 1, as it does on a refusal. Report that count to the architect before
  you spend your one re-run of the failed workflow; the architect sends the issue back so the
  implementer submits or discards its pending review. Once the review is submitted, the
  implementer's reply is the thread's newest comment, which leaves it open: answer the thread
  again, then run the command again. Once the review is discarded, your `Accepted:` is the newest
  comment again, and running the command again closes the thread.
  In a Legion pane (`LEGION_GRANT_FILE` is set), use `legion threads resolve --pr <number> --repo <owner>/<repo>`:
  outside the reviewer's pane it acts as your role's App from the gh files under `GH_CONFIG_DIR`.
  Outside a Legion pane, add `--gh` to that command, which applies the fallback's rule below through
  your own `gh`; where no `legion` command is installed, use `gh api graphql` with the session's
  GitHub credential and the fallback below.
  In a Legion pane, the **implementer** runs the command after every push that answers a review
  and before its `handoff_complete`, and pastes its output, stamped with the head it just pushed, into
  the `Threads` section. The output is then recorded against the head the reviewer will read, and
  nothing reads thread state before the implementer's completion. The command resolves each
  unresolved thread whose newest submitted comment is the opener's own `Accepted:` reply. On a
  thread a bot account opened that is none of Legion's role Apps (`LEGION_IMPLEMENT_APP_LOGIN` and
  `LEGION_REVIEW_APP_LOGIN` name them in every tree pane; the daemon's own route knows them for the
  reviewer; with either unset, no thread counts as a bot's), the Legion reviewer's `Accepted:` also
  closes it. GitHub cannot tell a CI bot, which
  never accepts, from a person whose `gh` is routed to an App, so the reviewer adjudicates such a
  finding, and it may accept one an App-routed person raised. The subject of a finding never
  closes it: the implementer's `Fixed in <commit>: …` or `Declined: …` answers a thread and closes
  none. A thread either Legion App opened, a reviewer's finding included, still needs its opener's
  `Accepted:`. It makes one `resolveReviewThread` per
  thread, prints `resolved <url> — <whose acceptance>` (its opener's, or the Legion reviewer's on a
  bot's thread, so the ledger shows which) or `left open <url> — newest reply by <login> is …`
  naming why, and exits 1 naming the thread's URL and GitHub's message when GitHub refuses one.

  Outside a Legion pane, page through `reviewThreads`, skip `isResolved: true`, and compare the opener
  with the newest comment. Query shape, inside `repository { pullRequest { … } }`:

  ```graphql
  reviewThreads(first: 100, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id isResolved
      opener: comments(first: 1) { nodes { author { __typename login } } }
      newest: comments(last: 1) { nodes { author { __typename login } body state } }
    }
  }
  ```

  Resolve only when the newest comment is submitted, its `author` is the opener's account (the same
  `__typename` and `login`: a login alone is a string anyone may register), and its `body`, after
  removing leading spaces, tabs, CR, and LF, begins `Accepted:`. Outside a Legion pane nothing
  names Legion's own App logins, so this route closes a bot's thread only on its
  opener's `Accepted:`: leave one the Legion reviewer accepted for the implementer's or merger's
  run in a pane, or report it. For each thread to resolve:

  ```graphql
  mutation($threadId: ID!) {
    resolveReviewThread(input: { threadId: $threadId }) { thread { isResolved } }
  }
  ```

  Re-read `reviewThreads` and confirm that thread's `isResolved` is true. In either route, report
  a refused resolution to the architect, which opens an ask for a human to resolve the thread by
  hand — never skip it silently. The merger runs the command once more before its READY
  completion and does not complete while any `left open` line remains. That run is where every accepted
  thread's resolution is guaranteed, since the merge queue's gate counts the unresolved threads at
  the head. Acceptances posted after the implementer's last run are resolved here.

- **The reviewer, on a re-review.** When you re-review after a corrective push, answer every
  thread you opened, and every thread a bot opened that is none of Legion's role Apps, in one of
  the three forms above — `Accepted:` is the only reply `legion threads resolve` acts on. A bot's
  finding you cannot accept becomes your own: leave it `Still open:` and request changes.
  Approve once each of those threads has your own `Accepted:` as its newest submitted comment,
  whether or not GitHub shows the thread resolved yet, and every other unresolved thread its
  opener's (read the newest comments with `gh api graphql`, never from the PR body). Another
  opener's thread that a person resolved with GitHub's button, with no `Accepted:`, gates nothing:
  neither `legion threads resolve` nor the merge queue's gate counts a resolved thread. Resolution
  is the pull request author's App's, so your approval never waits on it, except when a review
  workflow the project declares (`projects.<KEY>.review_workflows`) is red on its findings: such a
  workflow passes on a re-run only once its threads are resolved, so you run `legion threads
  resolve` (the daemon resolves the bot threads you accepted) and re-run the failed run before you
  approve, as your role prompt says.
  The merger resolves accepted threads that remain open before its READY completion.
