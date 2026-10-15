---
title: "A grant's staleness is decided at its mint, and a column a migration defaults reads as unrecorded for every existing row"
category: daemon
tags:
  - grants
  - redeem
  - migration-default
  - controller
  - stale-caller
date: 2026-10-13
status: active
module: packages/daemon/internal/api
related_issues:
  - "LEGION-164"
  - "sjawhar/legion#1932"
---

# A grant's staleness is decided at its mint, and a column a migration defaults reads as unrecorded for every existing row

- A refusal that reads a record the caller is identified by belongs at the mint, where the route
  knows who asked: `credential.Grant.Controller` is one flag for two callers — the controller
  session's mint (`controllerSessionGrant`, which proves the controller's secret) and the operator
  bearer's (`legion status --operator-token-file`, which proves nothing about the controller) — so
  a redeem-time check against the controller record refuses the operator too. Grants live in
  process memory, die with a restart, and are revoked by the mint that precedes any registration
  that would change the record, so a redeemable grant was minted for a caller current at the time;
  there is nothing left for redeem to decide.
- Before gating on a column a migration adds with a default, name every row the default reaches
  at the first boot: `integer not null default 0` makes every pre-existing claim and controller
  record read as "unrecorded" until its next registration, and a gate that treats 0 as stale refuses
  callers that worked the day before — the operator's own commands included. Decide what 0 means
  (none recorded) and which callers may be refused on it before the migration ships.
- Run `skill://ce-simplify-code` before the tester: its reuse reviewer is the pass that found this
  one here, reading the grant route's two mint paths against the redeem.

## Evidence

LEGION-164's migration 0042 adds `controllers.plugin_contract integer not null default 0`; the
first draft of `redeem` (`internal/api/credentials.go`) read the controller record for every
`Controller: true` grant and refused a contract other than the daemon's. At the first boot on the
release every deployment that had ever minted a controller capability would have answered its
operator's `legion status` with `409 STALE_PLUGIN_CONTRACT … run legion controller start from the
release built with this daemon`. The simplify pass's reuse reviewer found it; `redeem` now returns
a controller grant without a record read, `controllerSessionGrant` keeps the mint-time refusal,
and `TestOperatorBearerGrantRedeemsDespiteTheControllerRecordsStaleContract` pins it.
