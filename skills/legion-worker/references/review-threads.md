# Review threads

Part of `skill://legion-worker`. Read it when you reply to, accept, or resolve a review thread,
or run `legion threads resolve`: the implementer after every push that answers a review, the
merger before READY, and the reviewer, who answers threads on every re-review and runs nothing.
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
  every Legion pull request (`packages/daemon/src/daemon/AGENTS.md`, GitHub Apps).
  When `LEGION_GRANT_FILE` or `LEGION_GRANT` is set, use `legion threads resolve --pr <number> --repo <owner>/<repo>`.
  When neither is set, add `--gh` to that command, which applies the fallback's rule below through
  your own `gh`; where no `legion` command is installed, use `gh api graphql` with the session's
  GitHub credential and the fallback below.
  In a Legion pane, the **implementer** runs the command after every push that answers a review
  (the corrective push, and the final `.legion/` deletion push where the daemon has one) and
  before its `handoff_complete`, and pastes its output, stamped with the head it just pushed, into
  the `Threads` section. The output is then recorded against the head the reviewer will read, and
  nothing reads thread state before the implementer's completion. The command resolves each
  unresolved thread whose newest submitted comment is the opener's own `Accepted:` reply. On a
  thread a bot account opened that is none of Legion's role Apps (the daemon names them, keyed by
  App role), the Legion reviewer's `Accepted:` also closes it. GitHub cannot tell a CI bot, which
  never accepts, from a person whose `gh` is routed to an App, so the reviewer adjudicates such a
  finding, and it may accept one an App-routed person raised. The subject of a finding never
  closes it: the implementer's `Fixed in <commit>: …` or `Declined: …` answers a thread and closes
  none. A thread either Legion App opened, a reviewer's finding included, still needs its opener's
  `Accepted:`. It makes one `resolveReviewThread` per
  thread, prints `resolved <url> — <whose acceptance>` (its opener's, or the Legion reviewer's on a
  bot's thread, so the ledger shows which) or `left open <url> — newest reply by <login> is …`
  naming why, and exits 1 naming the thread's URL and GitHub's message when GitHub refuses one.

  Without a grant, page through `reviewThreads`, skip `isResolved: true`, and compare the opener
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
  removing leading spaces, tabs, CR, and LF, begins `Accepted:`. Without a
  grant nothing names Legion's own App logins, so this route closes a bot's thread only on its
  opener's `Accepted:`: leave one the Legion reviewer accepted for the implementer's or merger's
  run in a pane, or report it. For each thread to resolve:

  ```graphql
  mutation($threadId: ID!) {
    resolveReviewThread(input: { threadId: $threadId }) { thread { isResolved } }
  }
  ```

  Re-read `reviewThreads` and confirm that thread's `isResolved` is true. In either route, report
  a refused resolution to the architect, which opens an ask for a human to resolve the thread by
  hand — never skip it silently. The merger runs the command once more before publishing READY
  and does not publish while any `left open` line remains. That run is where every accepted
  thread's resolution is guaranteed, since the merge queue's gate counts the unresolved threads at
  the head. Where no `.legion/` deletion push follows the last review round (the Go daemon, before
  Stage 7), the reviewer's `Accepted:` replies to that round come after the implementer's last
  run, and this run is the only one that resolves them.

- **The reviewer, on a re-review.** When you re-review after a corrective push, answer every
  thread you opened, and every thread a bot opened that is none of Legion's role Apps, in one of
  the three forms above — `Accepted:` is the only reply `legion threads resolve` acts on. A bot's
  finding you cannot accept becomes your own: leave it `Still open:` and request changes.
  Approve once each of those threads has your own `Accepted:` as its newest submitted comment,
  whether or not GitHub shows the thread resolved yet, and every other unresolved thread its
  opener's (read the newest comments with `gh api graphql`, never from the PR body). Another
  opener's thread that a person resolved with GitHub's button, with no `Accepted:`, gates nothing:
  neither `legion threads resolve` nor the merge queue's gate counts a resolved thread. Resolution
  is the pull request author's App's, so your approval never waits on it. Where no `.legion/`
  deletion push follows your last round (the Go daemon, before Stage 7), the next run is the
  merger's before READY, a phase that starts only after your approval.
