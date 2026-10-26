---
title: "An evidence passage is a claim about runs: check each entry against them"
category: best-practices
tags:
  - documentation
  - verification
  - kubernetes
  - evidence
date: 2026-10-25
status: active
module: docs
related_issues:
  - "INFRA-234"
  - "sjawhar/legion#2182"
---

# An evidence passage is a claim about runs: check each entry against them

Extends docs/solutions/best-practices/publishing-a-command-sequence-a-stranger-can-run-against-a-cluster-INFRA-71.md.

That document covers how to run a published sequence. This one covers the passage that says what
has been run. Four rules:

1. **A list of what has not run is a claim about every entry in it.** When a later run lands,
   check the list one entry at a time against every record of that run. Classify each entry by
   the behaviour it exercises, not by the name the earlier run gave it. An entry found in any
   later run leaves the qualifier, and an entry found in none stays. If you correct only the entry
   someone pointed at, the rest of the list keeps claiming things nobody checked. When you correct
   such a list, say in the pull request that you checked it entry by entry, so the reviewer can
   check the method and not only the result.
2. **Do not claim how many attempts a check took unless the check printed each attempt.** A
   retry loop that prints only its final `answered` proves the address answered within the
   deadline. It does not prove the first try worked. Before you write "first try", "at once" or
   "after one refusal", add one print per attempt with a timestamp and the error class, and say in
   the evidence that you added it.
3. **When a sibling issue applies a policy that a published recipe depends on, run the recipe
   against it from the reader's own identity.** `kubectl auth can-i` answering `yes` shows the
   identity may create the object. It does not show that admission accepts it. Pair the corrected
   manifest with a `kubectl create --dry-run=server` of the old form. When the old form is
   refused, naming the rule, that is the negative control.
4. **A listing right after a delete shows the deletion still in progress.** Say what it shows,
   not what is left once cleanup finishes. Ask for every kind of object the delete should remove,
   or the listing cannot show that one went.

## Evidence

- Spec finding 5: a Service manifest published before the namespace's admission policy existed
  selected by `job-name`. Every gate answer was `yes`, and the manifest was still refused at
  submission. The policy requires `batch.kubernetes.io/controller-uid` at the owning Job's uid. A
  dry run of the old form (refused) beside the corrected form (admitted) proved the fix before the
  live run, at no cost.
- Rule 2: round 1 wrote that the reachability check answered "on its first try". The published
  block prints nothing per attempt. The tester added one print per attempt and saw the first
  connection refused and the retry answer about two seconds later. The listener opened about a
  second after the pod reported Ready, which is what the page's own paragraph about Ready and the
  listener predicts.
- Rule 1, two rounds in a row: the bullet for the parent issue's 2026-10-24 runs was headed "not
  run again since". Round 3 moved the Job half out from under it, because the bullet just above
  records the whole sequence running on 2026-10-25. Round 3 left the reachability check's listener
  case and refuses-then-listens case under it, though the 2026-10-25 runs had exercised both: two
  runs without a per-attempt print printed `answered`, and three with one showed a refusal, then
  an answer. Round 4 checked all five entries against the five run records. Three entries stayed:
  the gate with no credential, the Job-rule breakages and the dropped-packets case. Each of the
  four evidence defects in that pull request read as plausible on its own. In each case a run
  recorded in the same section contradicted it.
- Rule 4: right after `kubectl delete job`, `kubectl get jobs,pods,svc` showed the Job and its
  Service already gone and the pod `Terminating`. Within twenty seconds nothing was left. The
  listing the page published before this change did not ask for Services, so it could never have
  shown that the Service went with its Job.
