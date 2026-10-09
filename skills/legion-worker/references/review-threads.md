# Review threads

Part of `skill://legion-worker`. Read it when you reply on or resolve a review thread, list a pull
request's threads, or adjudicate a bot's finding: the implementer after every push that answers a
review, and the reviewer on every re-review. Every path it cites is in sjawhar/legion.

Three facts govern every thread, whoever opened it.

- **Reply on each thread you answer, one disposition reply per thread.** Every open review thread
  gets its own reply naming the fixing commit or the reason it isn't a defect: the implementer's
  `Fixed in <commit>: <one line>` or `Declined: <reason>`; the reviewer's reasons in its own words —
  the commit that fixed it, why it is not a defect, or what still stands. No word in a reply
  resolves anything, and nothing reads a reply for a magic form: a thread is resolved only by the
  resolution below, after its reply.
- **Resolve each thread you have answered, one thread per call, never in bulk, never a thread you
  have not read.** GitHub grants `resolveReviewThread` to the pull request's author, and the
  implementer opens every Legion pull request (`docs/site/src/content/docs/legion/running-legion.md`,
  "The two GitHub Apps"), so the two roles resolve differently:
  - The **implementer**, after every push that answers a review and before its `handoff_complete`,
    resolves each thread it answered with its own `gh`, one thread per call:

    ```bash
    gh api graphql -f query='mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}' -F id=<thread node id>
    ```

    A thread a bot opened (a CI bot's, or an App-routed person's) it answers the same way; the
    reviewer adjudicates the finding. When GitHub refuses a resolution, report the thread and
    GitHub's message to the architect with `envoy_publish`, which opens a `dispatch_ask` for a human
    to resolve it by hand; never skip it silently.
  - The **reviewer** cannot resolve a thread as its own App: GitHub refuses the review App
    `resolveReviewThread` on the implementer's pull request, so it may reply but resolves through
    the daemon. List the pull request's threads:

    ```bash
    gh api graphql -f query='query($o:String!,$r:String!,$n:Int!){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100){nodes{id isResolved comments(last:1){nodes{author{login} body}}}}}}}' -F o=<owner> -F r=<repo> -F n=<number>
    ```

    Reply on each thread you answer, then call the `legion` tool with `op: "resolve_threads"` and
    `threads: ["<node id>", …]`: the node ids of exactly the threads you answered — a bot's findings
    you adjudicated — never one you have not read. The daemon resolves exactly those as the
    implement App on the issue's recorded pull request, answers an id already resolved as `already
    resolved`, and refuses an id that is not a thread of that pull request
    (`THREAD_NOT_ON_PULL_REQUEST`) before writing anything. GitHub cannot tell a CI bot from a
    person whose `gh` is routed to an App, so such a thread may be a person's finding: resolve it
    only when the finding itself is settled (a genuine Minor is settled by being judged Minor),
    since the resolution says you judged it.
- **A reviewer who disagrees with a resolution replies and unresolves it.** Resolution is a
  judgment, not a ledger entry: a thread the implementer resolved whose finding still stands gets
  the reviewer's reply saying what stands and is unresolved, and a finding the reviewer cannot
  accept becomes its own, with a `REQUEST_CHANGES` naming it.

## The PR body's `Threads` section

The implementer records each thread it answered in the PR body's `Threads` section
(`skill://legion-worker/references/pr-body.md`), stamped with the head it just pushed: the thread's
id, its disposition (`fixed in <commit-sha> — <one line>` or `not a defect — <reason>`), and the
head that answered it. The reviewer verifies thread state in `gh api graphql`, never from that
section: it is the implementer's record, and GitHub is the fact.

## The reviewer, on a re-review

When you re-review after a corrective push, answer every thread you opened, and every thread a bot
opened that is none of Legion's role Apps, with one reply in your own words. A bot's finding you
cannot accept becomes your own: say what stands and request changes. Approve once each of those
threads carries your answer and no finding stands, read in `gh api graphql` — quote those answers
in the approval. Your approval never waits on a resolution: resolution is the pull request author's
App's, except when a review workflow the project declares (`projects.<KEY>.review_workflows`) is
red on its findings. Such a workflow starts again only on a push, so neither a reply nor a
resolution re-runs it, and it passes on a re-run only once its threads are resolved: answer its
threads, resolve the ones you answered with `resolve_threads` before you push your handoff, so the
run that push starts reads the threads as you left them, and re-run the failed run once, as your
role prompt says, before you approve. The merger resolves nothing: READY requires no thread state
beyond what your approval already judged.
