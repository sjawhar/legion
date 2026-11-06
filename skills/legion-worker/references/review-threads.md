# Review threads

Part of `skill://legion-worker`. Read it when you reply on or resolve a review thread, list a pull
request's threads, or adjudicate a bot's finding: the implementer after every push that answers a
review, and the reviewer on every re-review. Every path it cites is in cybermonklabs/legion.

Three facts govern every thread, whoever opened it.

- **Reply on each thread you answer, one disposition reply per thread.** Every open review thread
  gets its own reply naming the fixing commit or the reason it isn't a defect: the implementer's
  `Fixed in <commit>: <one line>` or `Declined: <reason>`; the reviewer's reasons in its own words —
  the commit that fixed it, why it is not a defect, or what still stands. No word in a reply
  resolves anything, and nothing reads a reply for a magic form: a thread is resolved only by the
  resolution below, after its reply.
- **Resolve each thread you have answered, one thread per call, never in bulk, never a thread you
  have not read.** The reviewer resolves: a thread it opened is a finding it judged, and the
  resolution records that judgment. The implementer resolves none; it replies, and it fixes.
  - The **reviewer** lists the pull request's threads, replies on each one it answers, and
    resolves it once it has read what closed it. List them:

    ```bash
    gh api graphql --paginate -f query='query($o:String!,$r:String!,$n:Int!,$endCursor:String){repository(owner:$o,name:$r){pullRequest(number:$n){reviewThreads(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{id isResolved comments(last:1){nodes{author{login} body}}}}}}}' -F o=<owner> -F r=<repo> -F n=<number>
    ```

    `--paginate` follows the thread pages, which it prints as one JSON object per page, so add
    `--slurp` when you want a single document; `comments(last:1)` needs no paging of its own.
    Then resolve, with your own `gh`, one thread per call:

    ```bash
    gh api graphql -f query='mutation($id:ID!){resolveReviewThread(input:{threadId:$id}){thread{isResolved}}}' -F id=<thread node id>
    ```

    A thread a bot opened (a CI bot's, or an App-routed person's) you adjudicate, answer and
    resolve the same way. GitHub cannot tell a CI bot from a person whose `gh` is routed to an
    App, so such a thread may be a person's finding: resolve it only when the finding itself is
    settled (a genuine Minor is settled by being judged Minor), since the resolution says you
    judged it. When GitHub refuses a resolution, report the thread and GitHub's message to the
    architect with `envoy_publish`, which opens a `dispatch ask` for a human to resolve it by
    hand; never skip it silently.
  - The **implementer** replies and resolves nothing. After every push that answers a review it
    replies on each thread that push answers, naming the fixing commit, and records each in the
    PR body's `Threads` section; a thread a bot opened it answers the same way, and the reviewer
    adjudicates that finding. It waits for no resolution before it completes: the reviewer reads
    the reply and the diff and resolves the thread in its round.
- **A resolution is a judgment, not a ledger entry.** Resolve a thread only once you have read
  what closed it, and leave open one whose finding still stands, whatever reply sits under it; a
  thread you resolved and then find standing again gets your reply saying what stands, and you
  unresolve it. A bot's finding you cannot accept becomes your own, named in the review: a
  `REQUEST_CHANGES` when that finding is behavioural, and otherwise a thread the approval leaves
  open.

## The PR body's `Threads` section

The implementer records each thread it answered in the PR body's `Threads` section
(`skill://legion-worker/references/pr-body.md`), stamped with the head it just pushed: the thread's
id, its disposition (`fixed in <commit-sha> — <one line>` or `not a defect — <reason>`), and the
head that answered it. The reviewer verifies thread state in `gh api graphql`, never from that
section: it is the implementer's record, and GitHub is the fact.

## The reviewer, on a re-review

When you re-review after a corrective push, answer every thread you opened, and every thread a bot
opened that is none of Legion's role Apps, with one reply in your own words. A bot's finding you
cannot accept becomes your own: say what stands. A reviewer submits `REQUEST_CHANGES` only when a
behavioural finding stands — one whose closing changes code that runs — and otherwise approves the
head it read, named by SHA, leaving every text finding as the inline review thread it already
writes. Approve once each of those threads carries your answer, read in `gh api graphql` — quote
those answers in the approval, and name there the threads it leaves open, which are the work the
merge waits for and what the implementer reads for its fix. Your approval never waits on a
resolution: the resolution of a text thread comes after the fix that closes it, and the section
below is that loop. A review bot that is red
while a thread it opened still stands is CI like any other, and
you re-run nothing: when the base branch requires its check, that red sends the work back to the
implementer, who answers or fixes each of the bot's threads and pushes, the bot runs again on
that push, and you resolve each of those threads in the round that reads it. The
merger reads no thread state and resolves none, and READY requires none beyond what your approval
already judged; a gate that does read one is the base branch's own GitHub rules, never Legion's
(`skill://legion-worker/references/merge-gate.md`).

## After an approval that leaves text threads open

The approval stands, and the issue moves to `accepting`, where the architect reads it. The
threads the approval names are the work the merge waits for, and this loop closes them.

1. **The implementer fixes the text and pushes it, once.** Its session is live from its own phase
   until the issue closes, so the approval reaches it there and it makes the fixes while the
   architect reads the issue for acceptance: it runs in no phase, writes no handoff, and makes
   one push of one commit of the text fixes. It replies on each thread, naming that commit, and
   resolves none.
2. **The reviewer resolves each thread it opened** once it has read the fix, one call per thread,
   and then approves the head the fix landed on, by SHA. That second approval is the one the
   merge gate reads, and its CI read is the read of that head's checks
   (`skill://legion-worker/references/merge-gate.md`).
3. **A thread the fix cannot close is a message to the architect**, sent with `envoy_publish` to
   its role topic by whichever role found it: the sentence is false because the code is wrong, or
   closing it would change code that runs. The architect moves the issue back to `implementing`.
   Neither the implementer nor the reviewer moves it.
