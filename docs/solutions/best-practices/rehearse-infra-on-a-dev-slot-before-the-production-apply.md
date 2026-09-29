---
title: Rehearse an infra change on a dev slot before its production apply
category: best-practices
tags:
  - deployment
  - dispatch
  - ecs
  - pulumi
  - testing
  - pre-merge-proof
date: 2026-09-13
status: active
module: dispatch
problem_type: process
---

# Rehearse an infra change on a dev slot before its production apply

## What happened

The ECS deployment of Dispatch (a Fargate service, an Aurora database, a bootstrap task and a
restore task) was designed, reviewed, merged through five PRs, and approved by Sami for its
production apply - and its first production apply failed at 00:09Z on the database bootstrap:
`ERROR: must be able to SET ROLE "dispatch"`. The Aurora master user is not a superuser, so
`CREATE DATABASE ... OWNER dispatch` needs the creator to be a member of the owner role. Every
production apply was blocked behind that failure until a one-line `GRANT` landed.

Nothing about that bug needed production to find it. A standalone development environment runs
the same Aurora (RDS master, non-superuser) and the same bootstrap task; one apply there would
have printed the error twenty hours earlier. The bootstrap task also had no log driver, so the
production failure surfaced only as `exit status 3` and the real error had to be recovered by
re-registering the task definition by hand with awslogs.

## The rule (Sami, 2026-09-13 00:48Z, verbatim)

> This should be obvious: the agent that developed it is responsible for testing, and they
> should be responsible for testing before it gets to production. There's this problem where
> we keep thinking that the proper way to develop things is to merge them and then test it,
> and that's obviously unacceptable. They need to test everything in a production-like
> environment before merging, and it is the agent that develops the feature that is
> responsible for doing that. If there's anything blocking that, we need to fix it: if it's
> infrastructure, we need to fix it; if it's tooling, we need to develop it; if it's skills,
> we need to fix the skills. Maybe using Legion will help with this, but it should not require
> deploying to production to realize your feature doesn't work. Also, the agent that developed
> it should be responsible for testing in production.

## What to do

- An infra PR's packet carries a link to its proof on a production-like surface **before**
  it is READY: an apply to a standalone development environment carrying every piece of
  infrastructure the change touches, the health check that proves the service answers, and
  the operational path the change adds (a bootstrap, a restore, a migration) exercised end to
  end. A green unit suite and a green `preview` are not it - the Pulumi harness mocks every
  provider call, and `preview` never runs a `command.local.Command`.
- Rehearse against the service's real dependencies, not a subset: for Dispatch that is its
  database and the event plane it publishes to, so the rehearsal environment stands both up;
  budget ~45 minutes and real database cost, destroy when done. A restore is rehearsed with a
  copy of a development dump, never against production data first.
- A one-shot task that runs inside an apply gets a log driver on day one. A task that can only
  report an exit code has already failed its first operator.
- The user-facing-surface rule is the same shape: a PR that changes what a human sees links a
  run on a real stack (Playwright against the Go server + Postgres with migrations counts; a
  screenshot from that stack counts) - not a unit run.

## Where the reviewed process already said this

The infrastructure repository's own pre-launch gates (a full rehearsal) and its deployment
guidance ("deploying is not testing": an infra change is exercised on a development stack
before its PR is opened) both required exactly this. The failure was not a missing rule; it
was an agent (this one) treating a reviewed design and an approved apply as a substitute for
running it. The `opening-a-pr` gate is where the proof link is checked.
