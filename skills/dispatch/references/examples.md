# Before and after: an ask, a message, and a draft

`skill://dispatch` sends you here for worked examples: a decision written as clickable options, a
message that should not be sent, and a draft placed where the human reads it.

## Before / after

Before — a wall of text hides the decision and makes the choices unclickable:

```text
We need to settle the release gate because the deploy branch has the migration and the
dashboard changes, I checked the staging result and it is fine except the release notes are
not reviewed, so should we ship today, wait for docs, or cut the dashboard from this release?
I think waiting is safest but the customer demo is tomorrow and the list above is probably stale.
```

After — anchor the decision and make each option a button:

```ts
dispatch_ask({
  issue: "LEGION-815",
  question:
    "Choose the release gate. Recommendation: ship after release-note review, since the tested deployment is otherwise ready.",
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
dispatch_ask({ issue: "OPS-52", question: "Send the customer update as drafted?", options: [...] })
```

After — the draft is a section of the spec, and the ask anchors there. If it really must be a
file (something to send as-is), the spec and the ask both link the slug from the upload result:

```ts
dispatch_doc_edit({ issue: "OPS-52", artifact: "spec", ops: [
  { op: "insert", after: "## Context", markdown: "## Draft\n\nHi team, ..." },
]})
dispatch_ask({
  issue: "OPS-52",
  question: "Send the customer update as drafted?",
  options: [...],
  anchor: { artifact: "spec", quote: "Hi team," },
})
// or, for a real file — the spec links it where the reader needs it, and so does the ask:
dispatch_doc_edit({ issue: "OPS-52", artifact: "spec", ops: [
  { op: "insert", after: "## Context", markdown: "## Draft\n\nThe update to send as-is: dispatch://OPS-52/artifact/cu-update-2026-09-15-md" },
]})
dispatch_ask({
  issue: "OPS-52",
  question: "Send this customer update as-is? dispatch://OPS-52/artifact/cu-update-2026-09-15-md",
  options: [...],
})
```
