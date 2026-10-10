---
title: "A lenient wire field that feeds a strict state schema is floored and put on one line where it is normalised"
category: daemon
tags:
  - strict-schema
  - nonEmptyString
  - normalisation
  - legion-state
  - control-characters
  - prompt-injection
  - run-and-report
date: 2026-10-10
status: active
module: packages/daemon/internal/capabilities
related_issues:
  - "LEGION-663"
  - "sjawhar/legion#1874"
---
# A lenient wire field that feeds a strict state schema is floored and put on one line where it is normalised

Extends docs/solutions/legion/a-strict-schemas-new-required-field-is-censused-across-every-parser-rigs-and-stand-ins-included-LEGION-578.md.

- That note censuses a new required field across every parser. The converse trap: a request
  field the daemon deliberately never refuses (a report, a detail string; "run, and report") that
  the daemon copies into `GET /legion/v1/state`, whose mirror in `@legion/contracts` is strict
  (`nonEmptyString`, bounded enums). One value the lenient side lets through and the strict side
  refuses — an empty string — fails the state parse for every plugin reader at once
  (`read_state`, `read_record`, `handoff_complete`'s record read), while the daemon answers 200 and
  persists the value across restarts. Where the daemon normalises such a field, floor every value
  the state schema would refuse (an empty failing detail → `no detail reported`) and keep the
  schema as the guard; test the floor in Go and the refusal in the contracts test, so the two
  sides are held together.
- A free-text value that reaches the daemon log, the state and the controller's prompt (the
  daily report quotes each open row's detail) is one line with its control characters out before
  it is kept: map `unicode.IsControl` runes to spaces, collapse whitespace with `strings.Fields`,
  then cut on a rune boundary. Names the worked repository chooses (an MCP server, a file under
  `.omp/`) are in such text, and a newline in one would end a log line or start a line in another
  session's instructions.
- A connection error string may quote a URL whole, userinfo included. Redact `://user:pass@` to
  `://***@` on the producer side, before the string becomes a detail that travels to Postgres,
  the log and the state.

## Evidence

LEGION-663's `capabilities.Normalize` cut a detail at 1024 bytes and filled a missing row, and
`api.CapabilityReportViewOf` copied a failing row's detail into `open[].detail`, which
`legionCapabilityReportView` declares `nonEmptyString`. The reviewer reproduced the break from the
contracts `state.json` with one open detail blanked (`…capabilities.open.0.detail: Too small`) and
named the producer path (`failed(firstLine(messageFor(error)))` on an error with an empty
message). The fix (`59dac701`) added `cleanDetail` and the `no detail reported` floor; the tester
drove a planner's ready with an empty failing detail, a detail carrying `\n`, `\t` and `\x1b`, and
a 2 KiB detail through a real `legion start` on Postgres 16.4: every ready 204, the state parsed
by the plugin's strict reader, the rows reading `no detail reported`, `not connected: evil server
[31m (boom) second line`, and a 1024-byte cut, one log line each.
