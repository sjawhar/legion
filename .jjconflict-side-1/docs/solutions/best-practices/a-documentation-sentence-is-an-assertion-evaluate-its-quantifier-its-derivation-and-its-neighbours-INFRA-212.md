---
title: "A documentation sentence is an assertion: evaluate its quantifier, its derivation and its neighbours before you push it"
category: best-practices
tags:
  - documentation-drift
  - proof
  - review-rounds
date: 2026-10-24
status: active
module: docs/kubernetes.md
applies_when:
  - You are writing a sentence that states what a command returns, or what is set where
  - A documented value could be derived two ways that agree on the deployment you can reach
  - You are adding prose beside a table that already states the same facts
related_issues:
  - "INFRA-212"
  - "sjawhar/legion#2152"
---

# A documentation sentence is an assertion: evaluate its quantifier, its derivation and its neighbours

A sentence in a runbook has a truth-condition, and a reader disproves it with one command. Three
gates, each run before the push, not after a round:

- **Evaluate every quantifier over the enumerated set and write the partition it produced.** A
  sentence containing *all*, *none*, *every* or *no container* is not written until a command has
  split the named set and the sentence states the split. "Prints them all" is a claim about
  eleven names; "prints each of them the runtime sets, and exits 1 for the four it does not" is
  the measurement. A loop whose output cannot fall two ways has proved nothing.
- **Write the derivation the code performs, not the value your deployment shows, whenever two
  candidate derivations agree there.** The pre-flight test is one question: *name a second input
  under which the two readings differ.* If you can, run the derivation with that input — a unit
  fixture is the right surface, and the only one. If you cannot construct it, say in the proof
  that no live surface settles the claim rather than citing an observation that would have agreed
  with the wrong sentence too.
- **Diff a new sentence against its nearest neighbours before you diff it against the code.** The
  table four lines below it, and the section its own pointer sends the reader to, are the
  highest-yield check and the one nobody runs, because proximity reads as consistency.

## Evidence (INFRA-212, sjawhar/legion#2152)

One paragraph of `docs/kubernetes.md` took three implementing rounds, each a different instance of
the same failure — a sentence treated as a description when it was an assertion.

**The quantifier.** Round 1 wrote "a `printenv` run by `kubectl exec` into the container finds
none of them", true of ten of the eleven names and false of `PATH`, which the *worker image's* own
`ENV` sets (`packages/daemon/docker/worker.Dockerfile`) rather than the runtime — and `podPath`'s
own comment says so. The tester disproved it in one command from inside a role container
(`tr '\0' '\n' < /proc/1/environ | grep '^PATH='`, PID 1 being the kubelet-started launcher, since
this pod's ServiceAccount cannot `kubectl exec`). The same paragraph's other half, "one the role's
own agent runs prints them all", survived into round 3: the agent prints seven and exits 1 for
four — the four rows of the table immediately below that read "the runtime sets it in no
container".

**The derivation.** The sentence said the daemon-launched controller's `GH_CONFIG_DIR` is "the
project's default owner's directory". The runtime never derives it that way: the controller's
launch spec carries no repository, so `appauth.GitHubCredential` takes
`bundle.Default = names[0]` over `slices.Sorted(maps.Keys(all))` — the alphabetically first owner.
Both readings name real things, and on this project both evaluate to `sjawhar`, so **no container
read could separate them**; the proof had to be the derivation executed with owners that do
differ (`{sjawhar, Acme}` → `acme`, and `{zebra-corp, acme}` → `acme` in the tester's own probe),
with the discriminating control being the same function asked for a named owner, which returns
that owner instead.

**The neighbours.** Both surviving defects contradicted something within four lines or two
sections: the table's four "sets it in no container" rows, and this same file's existing
statements that the controller's `github-default` is "the first owner". The repository commits no
check over this prose by decision, so the three gates above are the whole guard.
