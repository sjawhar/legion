---
title: "A bound tool assertion reads the argument that carries the call's target, never the model's intent, and a fix to a class of hole covers every tool the binding reaches"
category: testing
tags:
  - live-proof
  - stage4b
  - oh-my-pi-session
  - tool-results
  - jq
  - false-pass
date: 2026-10-16
status: active
module: scripts/e2e/lib/stage4b-tools.jq
related_issues:
  - "LEGION-780"
  - "sjawhar/legion#1982"
---

# A bound tool assertion reads the argument that carries the call's target, never the model's intent, and a fix to a class of hole covers every tool the binding reaches

Extends docs/solutions/testing/a-live-proof-reads-what-a-tool-returned-paired-to-its-call-and-binds-each-assertion-to-the-call-it-judges-LEGION-578.md.

- Oh My Pi puts the model's free-text `i` intent in every tool's arguments, so a binding that
  matches the arguments as JSON text is satisfied by the intent alone. Bind each tool on the
  argument that carries its target and never on `i`: bash on `command`, read on `path`, every
  other tool on its arguments object with `i` removed (`del(.i) | tojson`), which is a device
  write's content unchanged. State the rule once, in the def that every bound assertion and every
  fail-line quote reads (`arguments_carry` in `scripts/e2e/lib/stage4b-tools.jq`).
- Name a per-tool target only after reading the argument shapes the tool records, from a real
  session (`jq '.message.content[]? | select(.type == "toolCall") | {name, keys: (.arguments | keys)}'`
  over a fixture or your own session file). Oh My Pi's edit has `{input, i}` and no `path`; a
  write's stored arguments are its content, since `omp-tool-calls.jq`'s `call_arguments` reads every
  write as a possible device write. A rule that binds edit or write on `path` is unsatisfiable, not
  wrong-looking: the fail line would say "no edit result carries …" with no diagnosis.
- A hole in a shared mechanism is closed over the mechanism's whole domain, not at the call site
  where it was seen. One `arguments_carry` serves every bound tool; a fix that special-cases bash
  and keeps a generic else arm leaves the identical hole for read (a file-path read whose intent
  names `skill://<name>` passes the `skill://` binding) and for every tool called by name. Add a
  unit case per bound tool whose fixture carries the target in the intent beside a different real
  argument, which must fail.
- Re-prove a tightened binding over the real sessions that passed its earlier form, not only over
  composed fixtures. When the kept sessions live in a sibling issue's pod, ask that issue's live
  role over Envoy to attach them to its issue with `dispatch artifact` and reply with each file's
  `sha256sum`; read them back with `dispatch doc-read --issue <KEY> --artifact <slug>`, keep only the
  lines starting `{` (the read prefixes a `File <name>: …` header line), and check the hash before
  judging. Composed excerpts prove the rule's shape; the real ones prove nothing real was lost.
- A kept excerpt that runs past the operation it proves fails a tool-wide negative for reasons
  outside the operation: cut it at the operation's last tool result (here the `FullAgentGh` task
  result) before running "no bash result carries the other App", and record both windows' hashes.

## Evidence

sjawhar/legion#1982 (LEGION-780), the fast-follow the LEGION-578 retro named. Round 0 bound bash on
`command` and kept the JSON-text match for every other tool; the reviewer's round-1 thread
(PRRT_kwDORFy7ds6tDgTj) reproduced the same false PASS on the bound read at
`stage4b-sandbox-tree.sh:3702` with `{path: ".omp/skills/fixture-omp-skill/SKILL.md", i: "Reading
skill://fixture-omp-skill through its file path"}`, and the architect ruled the whole class in scope
as the spec's item read to its purpose. The architect's "edit/write on their path" was read to the
measured shapes above (the fixture's `edit {i,input}`, this implementer's own session's
`write {content,i,path}` stored as `{}`), so both take the object arm. The two real `full-agent-tools`
excerpts #1866's tester kept were attached to LEGION-578 on request (38 lines, sha256 `b1da44a1…`;
78 lines, sha256 `b27c947e…`) and passed every bound assertion under jq 1.6, 1.7.1 and 1.8.2 at
each head; the 78-line one failed the other-App negative in both the old and the new form because
its window ran into the tester's later work, and passed once cut at line 24 (sha256 `8a246bdf…`).
The intent-only bash, read and codegraph excerpts were refused by the final def and accepted by
each earlier one under all three jqs.
