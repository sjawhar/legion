---
title: "Publishing a command sequence a stranger can run against a cluster"
category: best-practices
tags:
  - kubernetes
  - documentation
  - verification
  - rbac
date: 2026-10-24
status: active
module: docs
related_issues:
  - "INFRA-71"
  - "sjawhar/legion#2092"
---

# Publishing a command sequence a stranger can run against a cluster

Five rules for a documented sequence of commands, each one a defect that was paid for in review
rounds. The measurement that earned each rule is beside it.

## Extract the published block and run the extraction

A command block is not reviewed until it has been pasted into the target shell as one block, and the
block that is run is the one extracted from the published text by script, never a retyped copy. A
syntax check does not stand in for this: `bash -n` passes a block whose first command cannot find
its input, and passes every rule below.

> Measured: two separate defects in one published sequence survived careful reading by three roles
> and died on the first paste — a wait that exited 1 at once, and a wait that answered about the
> wrong object.

## Judge a wait by which object it named, not by its exit status

A selector-based wait answers about whatever matches at that moment. Record the object's own name in
the evidence, and bound the wait tightly, so that a pass cannot come from the obstacle having gone
away on its own between runs.

> Measured: re-running a sequence that ends in `kubectl delete job` made `kubectl wait --for=create
> pod -l job-name=<name>` exit **0 after one second on the previous run's terminating pod** — a pod
> keeps that label while it dies. The Ready wait then reported `condition met` while the pod that
> would serve was still `Pending`. Both waits exited the way a reader wants; neither was about the
> workload just submitted. With a per-run name, every shape — three re-runs back to back, a delete
> issued while the pod was still running, and a paused run — matched exactly one pod and both waits
> named it.

## Name per run, not per thing, wherever several actors share one identity

A name built from the work item alone collides between the actors working that item, and between one
actor's successive runs. Add a per-run suffix and substitute it into every manifest, every selector
and every address at once; a selector left behind on the old name points at nothing, and that failure
often reads like a different one.

> Measured: six roles share one issue and one service account here. Under a fixed name a second
> submission is refused (`patch` on Jobs is not granted), and everything after it acts on the first
> actor's live workload — the waits, the connection check and the closing delete.

## After creating a workload, wait for its pod to exist before waiting for it to be ready

`kubectl wait --for=condition=Ready pod -l <selector>` against a selector matching nothing does not
poll: it exits 1 immediately with `error: no matching resources found`. `--for=create` polls to its
timeout. The pair is required, in that order, and `--for=create` needs a recent kubectl — check the
image's pin before publishing a sequence that uses it.

> Measured: the single Ready wait, run where the sequence put it, exited 1 at once; `--for=create` on
> the same selector polled and timed out as asked. The corrected pair, with nothing at all between
> the apply and the wait: create met at 1 s, Ready at 87 s on a cold machine, both 0.

## State the grant for every verb the sequence uses, and remember that `apply` reads

Verbs are granted independently, so one verb's answer predicts nothing about another's: an identity
may `delete` a Job it may not `patch`. A gate that asks only "may I create this" passes while the
step still fails, because a client-side `kubectl apply` reads the live object before it creates one,
and the refusal names *retrieving current configuration* rather than looking like a grant problem.
Ask `kubectl auth can-i` once per verb the sequence runs, and twice for `apply`.

> Measured: `kubectl apply` of a Service ended `Forbidden … cannot get resource "services"` with no
> create attempted, while `kubectl create` of the same file was refused on the create. In the same
> matrix: `create`/`delete jobs`, `list`/`get pods` and `pods/log` granted; `patch`/`update jobs`,
> `get`/`list endpoints` and `get`/`list`/`create services` not.

## A name-matching lint cannot see a name it was never told about

A check that matches a token list finds the tokens it holds and nothing else, so its green is not
evidence that a class of name is absent. The sweep over the diff is the control and the lint is the
backstop, never the other way round. The sweep covers **every** file the change adds, handoff and
record files included: those are part of the public diff, and a name quoted inside a record of the
decision to keep that name out is still published.

> Measured: a private infrastructure name reached a public branch inside a plan record, where the
> repository's own private-name check cannot match it and the change's own proof had swept the
> non-record files alone. A common word needs its context in the pattern to be swept at all — the
> namespace name here matched 896 files bare, and none in a Kubernetes context.
