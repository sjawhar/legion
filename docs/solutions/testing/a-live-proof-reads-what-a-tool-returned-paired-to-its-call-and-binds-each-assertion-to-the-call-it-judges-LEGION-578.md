---
title: "A live proof reads what a tool returned, paired to its call, and binds each assertion to the call it judges"
category: testing
tags:
  - live-proof
  - stage4b
  - oh-my-pi-session
  - tool-results
  - jq
date: 2026-10-16
status: active
module: scripts/e2e/lib/stage4b-tools.jq
related_issues:
  - "LEGION-578"
  - "sjawhar/legion#1866"
---

# A live proof reads what a tool returned, paired to its call, and binds each assertion to the call it judges

- Prove "the agent ran tool X and it answered Y" from the session's `toolResult` entries, never
  from the model's reply text: a model can quote a skill line it never read or a version it never
  ran. `assistant_said` proves what the model wrote; `tool_result_said` and `tool_result_answered`
  (`scripts/e2e/lib/stage4b-tools.jq`) prove what the tool returned.
- A result is the tool's by the call it answers, never by the `toolName` it records. Pair each
  result with its call by `toolCallId`, judge the call with `omp-tool-calls.jq`'s `calls(name)` (the
  tool by name, a `write` to its device `xd://<name>`, or `eval` code calling `tool.<name>(…)`), and
  carry the call's arguments onto the result. A write to `xd://lsp` records `toolName: "write"`; a
  name match would credit `write` and miss `lsp`.
- An error result never counts (`isError` true: a `bash` whose chain exited 1 after printing the
  wanted line, an `eval` that threw after the browser refused). A result whose call the session no
  longer holds is no tool's. A call whose `id` is no string is no call (`from_entries` would exit 5
  on it, and the failure surfaces as an ordinary "no result carries …" line with no diagnosis).
- Bind each assertion to the call that produced it, not to the tool: `tool_result_answered TOOL
  ARGS TEXT` requires ARGS in the call's arguments and TEXT in its result. `tool_result_said bash
  "$app"` is satisfied by `echo "$app"`; `tool_result_answered bash 'gh api graphql' "$app"` is not.
  Keep a negative tool-wide on purpose when the claim is "nothing of this kind anywhere" (the other
  App in no bash result; `notebook` never ran).
- ARGS is matched against the call's arguments serialised as JSON text, so a substring with a `"`
  or `\` can never match (the comparand is JSON-escaped), and a model-authored field in the
  arguments (Oh My Pi's `i` intent) is inside the matched string: an intent that reads "gh api
  graphql" beside another command satisfies the binding. Bind a bash call on its `command` alone
  when the binding must refuse that (the fast-follow this retro names).
- A wait for the agent's answer must key on a token no instructed tool call can contain:
  `assistant_said` also reads tool-call arguments, and an instruction whose eval code or edit
  content carries the nonce would be "answered" by the first call's emission, before any result
  exists. `full_agent_message` asks for reply lines starting `<nonce>-answer` and the wait keys on
  that suffix.
- Fetch each session once and keep the bytes as evidence; every assertion of the checkpoint reads
  that file (`session_jq SESSION …`). The bytes kept are the bytes judged, and a later round can
  re-prove a tightened assertion by replaying them instead of re-running the proof.
- Pin the library with a session a real pod wrote (`lib/testdata/stage4b-tools-session.jsonl`),
  holding each trap the rules above refuse: a device write recorded as `write`, a wanted string
  present only in an error result, a tool that never ran, a word the model wrote in a call's
  arguments, a null-id call beside a real one.

## Evidence

sjawhar/legion#1866 (LEGION-578) added Stage 4b's `repository-tools` and `full-agent-tools`
checkpoints over this library. The checked-in fixture is an implementer pod's own session: its Go
version appears only in a `bash` that exited 1 (a chained `gh api user` answered 403) and the
browser's refusal only in an `eval` that errored, and `lib/stage4b-tools.test.ts` refuses both; its
two `xd://lsp` writes are `lsp`'s results by their call's path. Review round 1 (thread
PRRT_kwDORFy7ds6tAG7q) found the App-login and toolchain assertions bound to the tool; the fix bound
them to `gh api graphql` and `go version &&`, and the implementer's control — an excerpt of its own
session holding only `echo 'legion-implementer[bot]'` — passed the old form and failed the new one,
while both of its real `full-agent-tools` sessions (the rehearsal and the tester's drive) passed the
bound forms. Round 2 re-proved every tightened assertion by replaying those kept sessions, with no
fifth run of a 13,294 s proof. The retro's fresh-eyes read found the `i`-field soft spot above: the
fixture's bash call carries `"i":"Finding an App-identity observable …"` beside its command, inside
the string `tool_result_answered` matches.
