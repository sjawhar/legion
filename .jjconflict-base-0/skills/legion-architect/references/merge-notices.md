# The merge notices: what each one means and what you do

Part of `skill://legion-architect`. Read the section for the notice that woke you. Each of
`merge-refused`, `merge-stalled`, `merge-removed` and `merge-waiting` is the daemon reporting on
the merge it submitted for an accepted READY, and each reaches the architect that owns the issue.
The wake-routing table, in `skill://legion-architect/references/wake-routing.md`, gives each
notice its move in one sentence; the section here is what that sentence stands on — what the
notice's `reason` names, what the daemon has and has not stopped doing, and what you read before
you ask a person.

In every one of the four the issue stays in `awaiting_merge`, where no worker holds a phase and a
backward move is refused, so what a repository rule needs is a person's to give.

## `merge-refused`

Its `reason` names the pull request, READY's head, what the daemon asked GitHub to do (merge,
enqueue, or enable auto-merge for it), and GitHub's refusal, which a retry would meet again
(squash merges disabled, a permission the implement App lacks). The daemon has stopped submitting
that merge; the issue stays in `awaiting_merge`. The cause is a repository setting or an App
permission, a person's to change: ask one with `dispatch ask`, naming the refusal and the pull
request at READY's head, which that person merges under the repository's rules once it is fixed.
The merge closes the issue. Never bypass a rule.

## `merge-stalled`

Its `reason` names the pull request, the state and head GitHub shows (closed, or another head),
since when GitHub has shown it, and READY's head, which the record still awaits. GitHub's last
change to the pull request is more than 10 minutes old and the record never heard of it, so its
webhook never reached the daemon: the daemon has stopped submitting that merge, and the issue
stays in `awaiting_merge`. Read the pull request on GitHub. Open at READY's head, GitHub has
caught up: a person merges it under the repository's rules, and the merge closes the issue.
Closed, or at another head the daemon never recorded: ask a person with `dispatch ask`, naming the
pull request, what GitHub shows, and READY's head. Never bypass a rule.

## `merge-removed`

Its `reason` names who took the pull request out of the merge queue or turned its auto-merge off
at READY's head after the daemon submitted it, or says GitHub turned it off on its own (over a
repository setting such as a merge method the repository blocks) and names the App that submitted
the merge, which GitHub names as the actor of its own disables; then what was removed, GitHub's
reason (`manual`, `merge_conflict`, `manually_disabled`, `merge_commit_blocked`, …), when, and
READY's head. It was not a failed merge, which comes as a `checks-red` instead. The daemon submits
that merge no further, so the removal stands, and the issue stays in `awaiting_merge`. Read the
pull request on GitHub. A `merge_conflict` conflicts with the base or with a pull request queued
ahead of it: once the conflict is on the base, the daemon's own conflict check sends the issue
back to `implementing` by itself. Otherwise ask a person with `dispatch ask` (the one who removed
it, when the reason names a person rather than GitHub on its own), naming the pull request at
READY's head and the reason; that person merges or queues it again under the repository's rules,
and the merge closes the issue. Never bypass a rule.

## `merge-waiting`

Its `reason` names the pull request, READY's head, how long the row has waited and what GitHub
last answered. GitHub has not completed the merge the daemon submitted within an hour of the
workflow queueing it at READY's acceptance. Nothing has been withdrawn: the merge stands, the
issue stays in `awaiting_merge`, and the daemon goes on submitting it, so there is nothing to undo
and you are told once, however much longer it waits. Read the pull request on GitHub and find what
GitHub is waiting for: a required check that never ran, a required check that failed, a review
only a person can give, or a merge queue that has not landed it. Act on what you find. In
`awaiting_merge` no worker holds a phase and a backward move is refused, so what a check, a review
or a repository rule needs is a person's: ask one with `dispatch ask`, naming the pull request at
READY's head and what GitHub waits on; the merge closes the issue when GitHub completes it,
whoever unblocks it. Never bypass a rule.
