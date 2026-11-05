---
title: "A setting another repository's deployment sets is removed in two releases: the reader warns and ignores first"
category: envoy
tags:
  - deletion
  - environment-variables
  - rollout
  - cross-repository
  - contract-census
date: 2026-11-03
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2674"
---

# A setting another repository's deployment sets is removed in two releases

Extends docs/solutions/testing/deleting-an-environment-seam-grep-its-tests-for-the-variable-a-fixture-that-still-injects-it-hides-the-dangling-reader-LEGION-631.md.

## The rule

Before you delete an environment variable, search the repository that deploys the process too:

```bash
gh api search/code -f q='<NAME> repo:<owner>/<repo>'
```

A variable another repository's Deployment sets is ignored with a warning, never refused at start.
A reader that refuses makes your release that deployment's outage, on a schedule you do not
control.

Warn once per variable, before any other work, so the lines are the first a reader of the log
meets.
Name the variable exactly as the environment spells it, say what the process stopped doing, and ask
for its removal from the deployment.
Log the value only when the variable holds no secret, and say in the code which it is.

File the pin move that removes the setter as its own issue, and give each removed name a rollout
line in the change's contract census: warn-first for a setting another repository owns, immediate
for a refusal whose production count is zero.

## Evidence

sjawhar/legion#2674 deleted the Envoy listener's `ENVOY_HOST_BRIDGE` and
`ENVOY_SUBSCRIBE_ALL_TOPICS`, with the push and the live topics lane they configured.

`gh api search/code` over `theorem-labs/agent-infra` found `ENVOY_SUBSCRIBE_ALL_TOPICS` in five
files, and the one that mattered is `envoy-cluster/listener.go:129`, which sets it to `1` as a
plain `corev1.EnvVarArgs` on the listener's own Deployment, with `envoy-cluster/main_test.go:870`
and `:884` asserting it.
Two more hits were comments in `Pulumi.prod.yaml` files and one was that repository's own history;
none was a stack input.
So the listener warns and serves: `warnIgnoredEnv` in `packages/envoy/cmd/listener/main.go`, called
for each variable immediately after `logging.New` and before any other work, with `os.LookupEnv` so
a variable set to the empty string still warns.
`TestAListenerStartedWithTheDeletedVariablesWarnsOncePerVariableAndServes` starts a real listener
process with both set and reads its log: exactly one warning for each, then the listener serves.
dispatch://INFRA-891 removes the setter with the image pin.

The contrast is in the same change's census.
The `port > 0` refusal is immediate, not warn-first, because the approved spec required it and
because `GET /v1/sessions` on the in-cluster listener counted 398 sessions and 0 with a port, read
the day the change was pushed, after 407 and 0 earlier that day and 377 and 0 the day before.
A refusal with a zero production count costs nobody; a refusal of a setting another repository
still writes costs that repository its deployment.
