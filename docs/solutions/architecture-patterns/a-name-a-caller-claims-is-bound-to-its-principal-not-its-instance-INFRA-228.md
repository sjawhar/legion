---
title: "A name a caller claims is bound to its principal, not its instance, and the rollout window is described rather than engineered away"
category: architecture-patterns
tags:
  - identity
  - token-exchange
  - multi-replica
  - rollout
date: 2026-10-25
status: active
module: packages/envoy/internal/broker
applies_when:
  - A caller declares an identifier that a token then carries and other systems trust
  - You are deciding what a reservation is keyed on, and for how long
  - Two replicas of one service can run different configurations at once
related_issues:
  - "INFRA-228"
  - "sjawhar/legion#2187"
---

# A name a caller claims is bound to its principal, not its instance

When a caller declares a name — a session id, a worker id, a lease name — and a credential then
carries it so other systems attribute writes to it, the name has to be bound at the point the
credential is minted. The three decisions that matter are what it is keyed on, how long the key
holds, and who decides when two configurations disagree.

- **Key the reservation on the principal, never on the instance.** The principal is the
  configured identity: the service account, the configuration entry, the person. The instance is
  the pod uid, the CI run, the process. Keying on the instance looks tighter and breaks every
  legitimate successor: the replacement pod of a rolling update, the next run of the same
  workflow, and a session resumed in a relaunched pod are all refused their own name until the
  dead instance's claim lapses. Those refusals are breakage, not security.
- **A lapse must never hand a name to a different principal.** "The holder stopped exchanging for
  one token lifetime" is not consent. Give the hold a retention long enough that no credential
  the system accepts can sit unused across it and still work, derive that bound from those
  credentials' own ceilings rather than from taste, and write what an operator sees and how a
  name is released early.
- **Make the claim from the issued credential's own expiry, inside the transaction that stores
  it.** A claim made from a lifetime computed before the library set the expiry can end before
  the credential it was made for.
- **A name the configuration fixes is reserved before its own caller first appears**, or a
  stranger takes it in the gap between deploying the configuration and the entry's first use.
- **When configurations can disagree across replicas, the configuration decides at the claim, not
  at a reservation taken once per process.** Each process honours its own configuration when the
  caller it designates arrives; reservation takes nothing from another principal. Otherwise the
  winner is whichever process happened to start last, and a replica restarted mid-rollout can
  leave the newly configured entry locked out of its own name indefinitely.
- **Describe the rollout window; do not engineer against it.** While two configurations are live
  the name alternates between the two entries each designates, bounded by how long the old
  generation serves. Ordering configurations needs a generation number an operator must set
  correctly on every change, a column to carry it and a comparison at every claim — and a
  generation that goes backwards pins the name permanently, which is the lockout the principal
  keying just removed. A bounded alternation that locks nobody out does not buy that.
- **Any once-per-process state is suspect the moment the service runs more than one replica.**
  When a change that adds replicas lands under your branch, re-check every `sync.Once`,
  process-local cache and limiter in your own diff before trusting the tests.

## Evidence

INFRA-228 (sjawhar/legion#2187) gave the secrets broker an RFC 8693 token exchange whose minted
token names the calling session. The session name is caller-declared, so the binding is the only
thing standing between a token and a forged attribution.

Each rule above cost a review or test round. Keying on the pod uid and the workflow run refused
the replacement pod and the next run (`400 invalid_grant`), reproduced by the reviewer on
Postgres. The first retention let a name pass to another project's pod one access lifetime after
its holder went quiet; the reviewer drove it and the newcomer's token introspected under the
victim's session. Reservation-takes-on-start produced a rollout in which an old-configuration
replica took a re-pointed name back and the new entry was refused until some replica restarted;
the tester found it with two real broker processes against one database, which no single-process
test could see. That race arrived because a change making the broker multi-replica landed on main
during a forward merge mid-round.

The final shape is one statement that refuses a claim (`BindSession`), one that reserves and takes
nothing (`ReserveSessions`), both in `packages/envoy/internal/broker/oauthstore/exchange.go`, and
`TestAFixedSessionNameAcrossARollout` in `packages/envoy/cmd/broker/multiprocess_test.go`, which
drives real processes through a rollout.

Two limits were taken knowingly rather than fixed: pods of one project resolve to one principal,
so inside a project a pod can still name another's session (dispatch://INFRA-382); and names are
first-come, so a principal allowed to exchange can claim a name another has not claimed yet.
A caller should therefore exchange at the start of a session, before its name is published
anywhere.
