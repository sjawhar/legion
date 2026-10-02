# Before and after: an ask, a message, and a draft

`skill://dispatch` sends you here for worked examples: a decision written as clickable options, a
message that should not be sent, and a draft placed where the human reads it.

## Before / after

Before — a wall of text hides the decision and makes the choices unclickable:

```text
The deploy branch has the migration and dashboard changes. Staging is fine, but release notes are
not reviewed before tomorrow's customer demo. Review the notes, ship without them, or cut the
dashboard from the release. Waiting is safest, but the list above may be stale.
```

After — state the problem and make each genuinely different option a button:

```ts
dispatch_ask({
  issue: "LEGION-815",
  question:
    "Release notes are unreviewed, so the release cannot pass its review gate before tomorrow's customer demo. How should we proceed? Recommendation: review the notes, then ship, to keep the release complete and reviewed.",
  options: [
    { label: "Review notes, then ship", description: "Keeps the release intact and reviewed." },
    { label: "Ship now", description: "Meets the demo deadline; release notes follow later." },
  ],
  urgency: "high",
  anchor: { artifact: "spec", quote: "Release requires reviewed operator instructions before deployment." },
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
    "Customers need an update today, but the draft is only in a separate file. How should we proceed? Recommendation: put the draft in the spec before sending it.",
  options: [
    { label: "Put the draft in the spec", description: "Lets the reader review the update in context." },
    { label: "Keep the separate file", description: "Leaves the reader to find the draft themselves." },
  ],
})
```

After — the draft is a section of the spec, and the ask anchors there. If it really must be a
file, the spec and the ask both link the slug from the upload result:

```ts
dispatch_doc_edit({ issue: "OPS-52", artifact: "spec", ops: [
  { op: "insert", after: "## Context", markdown: "## Draft\n\nHi team, ..." },
]})
dispatch_ask({
  issue: "OPS-52",
  question:
    "Customers need an update today, and the draft is ready in the spec. How should we proceed? Sending it reaches customers. Recommendation: send the reviewed update.",
  options: [
    { label: "Send the reviewed update", description: "Delivers the update to customers today." },
    { label: "Hold the update", description: "Leaves customers without the update until it is revised." },
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
    "Customers need an update today, and its reviewed text is in dispatch://OPS-52/artifact/cu-update-2026-09-15-md. How should we proceed? Recommendation: send the reviewed update.",
  options: [
    { label: "Send the reviewed update", description: "Delivers the linked update to customers today." },
    { label: "Hold the update", description: "Leaves customers without the update until it is revised." },
  ],
})
```
