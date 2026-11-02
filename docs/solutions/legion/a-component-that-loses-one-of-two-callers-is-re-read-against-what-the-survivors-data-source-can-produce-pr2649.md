---
title: "A component that loses one of two callers is re-read against what the survivor's data source can produce, not against its type"
category: legion
tags:
  - deletion
  - dead-code
  - shared-component
  - wire-types
  - review
date: 2026-11-02
status: active
module: packages/dispatch/web
related_issues:
  - "sjawhar/legion#2649"
---

# A component that loses one of two callers is re-read against what the survivor's data source can produce, not against its type

Extends docs/solutions/legion/deleting-a-gate-verify-each-survivors-behaviour-not-the-removed-symbols-absence-LEGION-630.md.
That rule says to verify each survivor's behaviour rather than the removed symbol's absence. This
one is the case where no symbol and no type changed at all: the survivor's **inputs** narrowed,
and the branches that served the deleted caller became unreachable while still compiling.

- **List what the surviving caller's data can actually hold, from the one route or query that
  produces it.** A shared component's branches are reachable only through its callers' data
  sources. Delete one caller and the remaining source bounds the component: read that source's own
  code — the SQL, the handler, the filter it selects on — and delete every branch no row it can
  emit will reach.
- **The type is no signal, and often must not change.** A wire type mirrors the response a service
  still sends, so a field the surviving route always leaves empty stays on the type while its
  rendering goes. Deleting by grep over a field name is the opposite mistake: the same name can
  live on a second type that a live route still fills.
- **Prose is part of the dead code.** Every doc comment and guide sentence that describes a deleted
  branch is false the moment it goes. Grep the component's name and the branch's words across the
  package's `AGENTS.md` and the sibling modules, and rewrite each in the same commit.
- **When no production input can reach the deleted branch, feed the impossible input by hand.**
  A throwaway probe that hands the surviving caller a record carrying exactly the fields the real
  source never sends, and observes that none of them renders while the page still works, turns "no
  test pins this absence" into an observed refusal.

## Evidence (sjawhar/legion#2649, INFRA-879)

Deleting Dispatch's `CredentialRecordPage` left `MachineLoginPage` the only caller of
`CredentialRecordFacts.tsx`. Its record comes from one route, `POST
/v1/machine-logins/lookup`, which selects `launcher_credential` records alone, and
`packages/envoy/internal/broker/requests/machine_read.go:785-790` fills the enrollment only for the
other kind:

```go
agentSecret := kind == record.KindAgentSecret
var enr *record.Enrollment
if agentSecret {
	e := body.Enrollment
	enr = &e
}
```

so the Enrollment fact, the Worker slot fact and the Session fact with its `useAgents` call could
no longer carry anything, and the `record.kind === "launcher_credential"` guard was always true.
All four compiled, and their only tests had gone with the deleted page. The reviewer found them as
the round's one Important finding.

The type was deliberately left alone in the same change: `CredentialRecord.enrollment` and
`.session` stay in `web/src/api/types.ts`, because the broker still sends both fields on every
record. The contrast is `approve_url`, which the same diff deletes from `CredentialRecord`, whose
only writer was the deleted route, and keeps on `CredentialPendingRow`, which the live pending list
still fills — a grep-and-delete of the field name would have broken the Inbox row.

Five prose sites described the removed branches as live and were rewritten with them: the
component's own doc comment, `packages/dispatch/AGENTS.md:851` and its machine-page paragraph,
`CredentialSessionLines.tsx:37` and `:50`, and `session.ts:23`.

The tester's control was the impossible input: a machine login handed an enrollment with `slot:
implementer-g3` and a session id, which that lookup never sends, with the page rendering neither
while every remaining fact rendered and Approve still decided the login.
