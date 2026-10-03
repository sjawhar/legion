# Before and after: a decision block, a reply, a message, and a draft

`skill://dispatch` sends you here for worked examples: a design decision in its document, a reply
that names its mechanism, a message that should not be sent, and a draft placed where the human
reads it.

## Before / after

Before — a wall of text hides the decision and the human never receives it:

```text
The deploy branch has the migration and dashboard changes. Staging is fine, but release notes are
not reviewed before tomorrow's customer demo. Review the notes, ship without them, or cut the
dashboard from the release. Waiting is safest, but the list above may be stale.
```

After — write the decision into the document section it concerns: state the problem, and make each
genuinely different option a button, so the answer stays with the release design:

```ts
dispatch_doc_edit({
  issue: "LEGION-815",
  artifact: "spec",
  ops: [{
    op: "insert",
    after: "Release requires reviewed operator instructions before deployment.",
    markdown: `:::ask{#release-gate urgency="high"}
Release notes are unreviewed, and tomorrow's customer demo means the release cannot wait for a later review. How should we proceed? Recommendation: review the notes, then ship, to keep the release complete and reviewed.

- Review notes, then ship: Delays release for review but keeps the release complete and reviewed.
- Ship now: Meets the demo deadline but leaves the release notes unreviewed.
:::`,
  }],
})
```

Before — a progress note that nobody needs, posted where humans look for decisions:

```ts
dispatch_message({ issue: "LEGION-815", body: "Merged the release PR, moving to docs next." })
```

After — nothing. The merge is visible on the pull request; the docs work shows up as its own deliverable. Post a message only when
a human must act or a deliverable is theirs to use:

```ts
dispatch_message({
  issue: "LEGION-815",
  body: "Release 1.4 is live on the devbox (dispatch://LEGION-815/artifact/release-notes). Nothing needed from you.",
})
```

Before — a draft the human must read is uploaded as a separate file, the spec only names it, and
the ask does not point at it, so the reader has to go looking:

```ts
dispatch_artifact({ issue: "OPS-52", name: "cu-update-2026-09-15.md", content: "Hi team, ..." })
dispatch_doc_edit({ issue: "OPS-52", artifact: "spec", ops: [
  { op: "insert", after: "## Context", markdown: "## Draft (artifact cu-update-2026-09-15.md)" },
]})
dispatch_ask({
  issue: "OPS-52",
  question:
    "Customers need an update today, but the draft is only in a separate file, so the reader cannot review it in context. How should we proceed? Recommendation: put the draft in the spec before sending it.",
  options: [
    { label: "Put the draft in the spec", description: "Adds a spec edit before sending but lets the reader review it in context." },
    { label: "Keep the separate file", description: "Saves the spec edit but leaves the reader to find the draft." },
  ],
})
```

After — the draft is a section of the spec, and the ask anchors there. Sending it is a to-do only a
human can complete, so it stays a standalone ask. If it really must be a file, the spec and the ask
both link the slug from the upload result:

```ts
dispatch_doc_edit({ issue: "OPS-52", artifact: "spec", ops: [
  { op: "insert", after: "## Context", markdown: "## Draft\n\nHi team, ..." },
]})
dispatch_ask({
  issue: "OPS-52",
  question:
    "Customers need an update today, and the reviewed text is ready in the spec. Sending it cannot be recalled. How should we proceed? Recommendation: send the reviewed update.",
  options: [
    { label: "Send the reviewed update", description: "Delivers the update today but makes its text external." },
    { label: "Hold the update", description: "Avoids sending now but leaves customers without the update." },
  ],
  anchor: { artifact: "spec", quote: "Hi team," },
})
// or, for a real file — the spec links it where the reader needs it, and so does the ask:
dispatch_doc_edit({ issue: "OPS-52", artifact: "spec", ops: [
  { op: "insert", after: "## Context", markdown: "## Draft\n\nThe update to send: dispatch://OPS-52/artifact/cu-update-2026-09-15-md" },
]})
dispatch_ask({
  issue: "OPS-52",
  question:
    "Customers need an update today, and its reviewed text is linked from the spec. Sending it cannot be recalled. How should we proceed? Recommendation: send the reviewed update.",
  options: [
    { label: "Send the reviewed update", description: "Delivers the linked update today but makes its text external." },
    { label: "Hold the update", description: "Avoids sending now but leaves customers without the update." },
  ],
  ref: "dispatch://OPS-52/artifact/cu-update-2026-09-15-md",
})
```

## Naming the mechanism

Before — a reply explaining a design paraphrases its tool, its route and its event away, so the
reader has to ask which tool, and how Legion "notices" anything:

```text
Today, when a spec is ready, the architect tells Legion so with a dedicated call on its Legion
tool, and Legion then waits for your approval. This change removes that call, because Legion will
notice your approval of the spec by itself.
```

After — the reader's sentence first, then each mechanism by its real name, with how it works in a
clause:

```text
For you nothing changes: you still click Approve on the spec. What goes is a step the architect
takes today: it calls `register_gate` on its `legion` tool, which posts to the daemon's
`POST /legion/v1/gates/register` route and names the spec document and the version that gate the
tree. The daemon opens the gate when Dispatch emits `artifact.approved` for that document at that
version, which Dispatch does when you click Approve on the request the architect opened with
`dispatch_request_approval`. Without `register_gate`, the daemon reads the issue's primary
document from Dispatch itself and waits for the same event.
```

"A dedicated call", "tells" and "notice" each stood for something the reader can search for, find
in a log, or set. None is a noun anyone coined, so each is named; the clause after each name is
what keeps the reply readable on a phone.
