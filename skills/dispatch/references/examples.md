# Before and after: a decision block, a reply, a message, and a draft

## Before / after

Before: a wall of text hides the decision, and the human never gets it.

```text
The deploy branch has the migration and dashboard changes. Staging is fine, but release notes are
not reviewed before tomorrow's customer demo. Review the notes, ship without them, or cut the
dashboard from the release. Waiting is safest, but the list above may be stale.
```

After: a decision block in the section it concerns, each real option a button.

```bash
dispatch doc-edit --issue LEGION-815 --artifact spec --ops-json-file - <<'EOF'
[{
  "op": "insert",
  "after": "Release requires reviewed operator instructions before deployment.",
  "markdown": ":::ask{#release-gate urgency=\"high\"}\nRelease notes are unreviewed, and tomorrow's customer demo means the release cannot wait for a later review. How should we proceed? Recommendation: review the notes, then ship, to keep the release complete and reviewed.\n\n- Review notes, then ship: Delays release for review but keeps the release complete and reviewed.\n- Ship now: Meets the demo deadline but leaves the release notes unreviewed.\n:::"
}]
EOF
```

Before: a progress note nobody needs.

```bash
dispatch message --issue LEGION-815 --body 'Merged the release PR, moving to docs next.'
```

After: nothing. The PR shows the merge. Post only when a human must act or can use a deliverable:

```bash
dispatch message --issue LEGION-815 --body 'Release 1.4 is live on the devbox (dispatch://LEGION-815/artifact/release-notes). Nothing needed from you.'
```

Before: the draft is a separate file, the spec only names it, and the ask does not link it. Each
write is its own call, as AskGate reads it: two writes in one call are refused.

```bash
dispatch artifact --issue OPS-52 --name cu-update-2026-09-15.md --content 'Hi team, ...'
```

```bash
dispatch doc-edit --issue OPS-52 --artifact spec --ops-json '[{"op":"insert","after":"## Context","markdown":"## Draft (artifact cu-update-2026-09-15.md)"}]'
```

After: the draft is a spec section, and the ask (a human-only send) anchors there. A real file is linked by its slug from both.

```bash
dispatch doc-edit --issue OPS-52 --artifact spec --ops-json '[{"op":"insert","after":"## Context","markdown":"## Draft\n\nHi team, ..."}]'
```

```bash
dispatch ask --issue OPS-52 --anchor-json '{"artifact":"spec","quote":"Hi team,"}' --question 'Customers need an update today, and the reviewed text is ready in the spec. Sending it cannot be recalled. How should we proceed? Recommendation: send the reviewed update.' --option 'Send the reviewed update: Delivers the update today but makes its text external.' --option 'Hold the update: Avoids sending now but leaves customers without the update.'
```

Or, for a real file, again one write per call:

```bash
dispatch doc-edit --issue OPS-52 --artifact spec --ops-json '[{"op":"insert","after":"## Context","markdown":"## Draft\n\nThe update to send: dispatch://OPS-52/artifact/cu-update-2026-09-15-md"}]'
```

```bash
dispatch ask --issue OPS-52 --ref dispatch://OPS-52/artifact/cu-update-2026-09-15-md --question 'Customers need an update today, and its reviewed text is linked from the spec. Sending it cannot be recalled. How should we proceed? Recommendation: send the reviewed update.' --option 'Send the reviewed update: Delivers the linked update today but makes its text external.' --option 'Hold the update: Avoids sending now but leaves customers without the update.'
```

## Naming the mechanism

Before: the reply paraphrases its tool, route and event away.

```text
Today, when a spec is ready, the architect tells Legion so with a dedicated call on its Legion
tool, and Legion then waits for your approval. This change removes that call, because Legion will
notice your approval of the spec by itself.
```

After: the reader's sentence first, then each mechanism by name, with how it works in a clause.

```text
For you nothing changes: you still click Approve on the spec. What goes is a step the architect
takes today: it calls `register_gate` on its `legion` tool, which posts to the daemon's
`POST /legion/v1/gates/register` route and names the spec document and the version that gate the
tree. The daemon opens the gate when Dispatch emits `artifact.approved` for that document at that
version, which Dispatch does when you click Approve on the request the architect opened with
`dispatch request-approval`. Without `register_gate`, the daemon reads the issue's primary
document from Dispatch itself and waits for the same event.
```

"A dedicated call", "tells" and "notice" each stood for something the reader can search, find in a log, or set.
