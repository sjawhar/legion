---
title: "A server-named object gets a client handle before the create"
category: best-practices
tags:
  - kubernetes
  - documentation
  - cleanup
  - generate-name
date: 2026-10-25
status: active
module: docs/kubernetes.md
related_issues:
  - "INFRA-232"
  - "sjawhar/legion#2195"
---

# A server-named object gets a client handle before the create

Extends docs/solutions/best-practices/publishing-a-command-sequence-a-stranger-can-run-against-a-cluster-INFRA-71.md.

That document's "Name per run" rule has the sequence make its own per-run suffix. Where the
namespace may hold names an operator declares, let the API server make the name instead, and add
the handle below. These are the rules on top of it:

1. **Take the name from the server with `metadata.generateName` and `kubectl create`.**
   `kubectl apply` refuses `generateName` on the client (`cannot use generate name with apply`).
   Read the name and the uid from the create's own answer, in one call:
   `created=$(kubectl create -o jsonpath='{.metadata.name} {.metadata.uid}' -f job.yaml)`.
2. **Before the create, write a value the client chose onto the object, and print it.** A name
   that only the create's answer carries does not exist for a stop that lands while the create is
   in flight or before its answer is read. A random per-run label on the object's own metadata
   (not on the pod template, so that no pod carries it) gives the trap and a person something to
   delete by when no name came back: `kubectl delete job -l <key>=<value>`.
3. **Set every variable the trap reads before you install the trap**, and give the trap a branch
   for "no name was read". Under `set -u`, a trap that reads an unset name fails. A trap that runs
   `kubectl delete job ""` fails `resource name may not be empty` and reports a failed delete of a
   Job that never existed.
4. **Quote every label value you substitute into a YAML heredoc.** Random hex can come out all
   digits ((10/16)^8, about one 8-character value in 43). YAML then reads it as a number, and the
   API refuses it (`cannot unmarshal number into Go struct field ObjectMeta.metadata.labels of type
   string`).

## Evidence

- Round 2 of sjawhar/legion#2195 used `generateName` with no label. A `kill -TERM` placed right
  after the create printed `cleanup: no Job name was read; nothing removed`, and the Job stayed
  `Running`. The tester got the same result from timed stops at 0.23 s and 0.24 s. The reviewer
  requested changes on this window: under the client-chosen name the window did not exist,
  because the name was known before the create.
- Round 3 added `sequence-run: "$run"` on the Job's metadata. The same stop gave `deleting by
  label`, the Job was deleted, and a concurrent run's Job ran on to `sequence: exit 0`. A `SIGKILL`
  inside the create printed no name, and `kubectl delete job -l sequence-run=<printed value>`
  removed the Job. No pod carried the label in any run.
- The tester's negative control: an unquoted `sequence-run: 12345678` in a server dry run was
  refused with the unmarshal error. Quoted, `12345678`, `00000010`, `1e100000` and `deadbeef` were
  all admitted as strings.
