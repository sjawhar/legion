---
title: "A refusal names one edit the caller may make, and a test follows it over every input"
category: best-practices
tags:
  - error-messages
  - refusal
  - exhaustive-testing
  - untrusted-input
date: 2026-10-29
status: active
module: packages/envoy
related_issues:
  - "sjawhar/legion#2433"
---

# A refusal names one edit the caller may make, and a test follows it over every input

Extends docs/solutions/best-practices/a-scanner-refuses-every-entry-it-cannot-handle-and-the-remedy-its-error-names-is-tested-too-INFRA-629.md.

- Two readers can disagree about one structure: here the server reads it leniently (it joins
  every list) and the card reads it by position (first list). Then no write path may rebuild the
  structure from the lenient reading. Refuse the write, before the edit is built. A rebuild moves
  or drops what another person wrote, and each patch to it produces the next review finding.
- Name one atomic repair. When the validator refuses each intermediate state, a sequence of edits
  cannot be followed.
- Name only an action this caller can always take. Never "edit object X first": X can be one that
  does not parse, is already decided, or is another person's, so the caller loops or gets 409/403.
  When no such action exists, say why and name the human surface, such as the document editor.
- Check the repair before you name it. Apply it in simulation, and name it only when every object
  it changes then passes the same strict check the edit route runs.
- Give ids and quoted content as quoted data (`strconv.Quote`, `%q`), never inside a ready-made
  command, because ids are document text. While ids are unsettled, name none and say to retry.
- Test the refusal by following it as written over an enumerated space of broken inputs. Apply
  exactly what it names, retry the original edit, and assert that the end state is valid and that
  no word was lost that the refusal did not quote.

## Evidence

- Rounds 1 to 6 patched a rebuild of a broken decision: two lists, missing options, repeated
  labels, nested items. The design change `ddf1bc5b` refused every such edit and closed seven
  threads.
- The "edit ask block X first" path looped and hit `ASK_NOT_OPEN`/`NOT_AUTHOR`
  (`packages/envoy/internal/dispatch/api/asks.go`). It was removed in `9712a697`.
- `packages/envoy/internal/dispatch/docs/ask_block_text.go:506` (`refuseAskContentEdit`, called at
  `:79` before the edit is built), `:589` (`planAskRepair`).
- `packages/envoy/internal/dispatch/docs/service_test.go:934` and `:1190` enumerate every body of
  up to five blocks, and of up to four with a nested decision. `:1732` (`followAskRefusal`)
  follows each refusal with one `ApplyOps`, then the ask edit. `:1851` (`sendsTheCallerOn`) fails
  any refusal that sends the caller to another object.
