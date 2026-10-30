---
title: "A stored value that can fail a read is guarded in the SQL cast and the Go decode, and answers like the null case in every reader"
category: database-issues
tags:
  - postgres
  - jsonb
  - robustness
  - dispatch
date: 2026-10-30
status: active
module: packages/envoy/internal/dispatch
related_issues:
  - "sjawhar/legion#2483"
---

# A stored value that can fail a read is guarded in the SQL cast and the Go decode, and answers like the null case in every reader

- A stored value that is not what the code expects can fail a route in two places: the SQL cast
  (SQLSTATE 22P02 fails the whole statement) and the Go decode (`cannot unmarshal number into ...`).
  Make both tolerate it, each through one shared helper, and search for every reader of the value.
- Before you make a bad value resolve to "nothing", read what each reader already answers when the
  value is null, and make the new case give that same answer in every reader. A `case when x is not
  null then <guarded cast of x>` branch is wrong: test the guarded expression for null, so the row
  falls through exactly as a null does.
- A read that must keep its answers guards with what the cast accepts (`pg_input_is_valid`), not
  with a narrower canonical form; only an index needs the narrower immutable guard.
- Build the test fixture from everything the column's type can hold, not from the one bad row that
  was reported: each spelling the cast accepts, each it refuses, and non-string JSON (number,
  `null`, object). Assert, reader by reader, that the canonical rows are unchanged and the bad rows
  agree across readers (for example the graph parent and the search link).

## Evidence

- In sjawhar/legion#2483 the issue's ask list, the single-ask read, the event log, both search
  arms, the references graph and the ask reload after an anchor refresh each cast an ask's
  `anchor->>'artifact_id'` unguarded; one stored `not-a-uuid` made `GET /api/v1/issues/{key}/asks`
  answer 500 on main. They now read through `store.CastableUUID`.
- A first round guarded them with the lower-case regex the index needs; upper-case and braced ids,
  which cast, stopped resolving, and the tester's spelling tests caught it.
- With the SQL fixed, an `artifact_id` stored as the JSON number `5` still answered 500 from the Go
  decode; `model.DecodeAnchor` fixed it (`TestAskRoutesAnswerOverAnAnchorArtifactIDThatNamesNoDocument`).
- `refs/graph.go` first tested `k.anchor->>'artifact_id' is not null` and then used the guarded cast,
  so an uncastable anchor got its issue as parent while search linked the same ask to its block
  document. It now builds the expression once (`refs/graph.go:614`) and tests it in both the version
  and document branches (`:619`, `:627`); `TestRefsAndSearchPutAnAskWhoseAnchorDoesNotCastOnItsBlockDocument`
  pins the agreement. The nine-spelling boot over the replay seed is the proof recorded on the PR.
