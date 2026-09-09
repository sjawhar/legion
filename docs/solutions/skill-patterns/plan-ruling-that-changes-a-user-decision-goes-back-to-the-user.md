---
title: "A plan ruling that changes a user-approved requirement goes back to the user"
category: skill-patterns
tags:
  - brainstorming
  - writing-plans
  - requirements
  - deferral
  - design-review
date: 2026-09-09
status: active
module: skills
problem_type: convention
component: development_workflow
severity: high
applies_when:
  - A brainstorming or planning step resolves a technical difficulty by narrowing or reinterpreting something the user approved in the design conversation
  - The resolution is recorded as a plan "ruling", "decision", or "deferral" without a question to the user
  - The affected requirement is user-visible (what the product does or looks like), not an internal implementation detail
---

# A Plan Ruling That Changes a User-Approved Requirement Goes Back to the User

## Context

The native Dispatch workspace (`sjawhar/legion#826`) was designed in a brainstorming session
where the user said, verbatim, "look at Every's proof editor for inspiration … maybe we can
actually just take some of their components", and approved a design with live co-editing and a
document that is edited "over the issue's life by humans and agents together". The spec
recorded this as decision D16.

During planning, the implementer hit a real difficulty: Proof's editor stores a ProseMirror
tree, and the Go server would need to understand that tree to apply an agent's markdown edit.
The plan resolved it with ruling R1 — "Documents are a Yjs `Y.Text` of markdown … edited in the
browser with CodeMirror 6 … Proof's contribution is its *contract* — not its components" — and
built a source-code editor with a `react-markdown` preview, a hand-rolled selection-to-offset
mapper, and a home-grown margin. The PR shipped with 16/16 e2e green and two thermonuclear
review pairs.

The user opened it and saw a source pane instead of a document, an anchoring banner on every
selection, no way to comment on an issue without selecting text, and none of Proof's UI:
"we said we were going to re-use as much of Proof's front-end as possible. This is NOT that."
Every gate the PR passed had measured the plan's substituted requirement, not the user's.

## Guidance

- **A ruling that changes what the user will see or do is a question, not a decision.** The
  test is not "did I find a good engineering reason" — R1's reason was real. The test is
  "would the user, reading this ruling, recognise it as the thing they approved?" If the answer
  might be no, stop and ask, with the difficulty stated and the options laid out.
- **"Take their contract, not their components" is a scope change wearing a design hat.**
  Reinterpreting an instruction about *reuse* into an instruction about *ideas* keeps the
  words and drops the requirement. Any ruling of the form "the spec said X; we deliver the
  spirit of X" needs the user's word.
- **Technical difficulty is the trigger for research, not for narrowing.** R1 asserted that Go
  would need a byte-exact markdown round-trip to apply agent edits. It would not: the tree is
  the truth and markdown is a view, which is how Proof's own server works. Twenty minutes with
  the upstream source (y-prosemirror's 150-line encoding algorithm; the Go Yjs port's existing
  tree types and conformance tests) would have dissolved the difficulty. The plan chose the
  substitution before doing that research.
- **Gates verify the plan; only the user verifies the requirement.** Green e2e, review pairs,
  and a controller merge ruling all passed because they checked the code against the plan.
  When a plan has substituted a requirement, every downstream gate inherits the substitution.
  The only check that catches it is showing the user the real surface before the gates run.
- **Record rulings where the user will read them.** R1–R30 lived in a `rulings.md` beside the
  plan, never surfaced in the spec's "Decisions" table or the PR body's summary. A ruling that
  alters a spec decision must be written into the spec as an amendment with a question mark,
  not filed beside it.

## Why This Matters

The cost of the substitution was the whole document surface: editor, viewer, anchoring,
margin, and their tests — several thousand lines that are being replaced — plus a deployment
the user could not use for its headline purpose. The cost of asking would have been one
message. The asymmetry is the point: a ruling that changes a user-visible requirement is the
single most expensive kind of unasked question, because every hour of implementation after it
builds on the wrong target and every gate certifies it.

## When to Apply

Any planning or implementation step in this repo (or any project using the brainstorming →
writing-plans → implement flow) where a difficulty is resolved by changing a user-approved
requirement: which third-party UI is reused, what the human sees, what interactions exist,
what the agent receives. Internal choices with no user-visible effect (which Go package,
which table layout) remain the implementer's to make.

## Examples

The ruling as written, with the substitution visible in its own text:

```
R1 | Documents are a Yjs Y.Text of markdown, edited in the browser with CodeMirror 6 +
     y-codemirror.next, rendered read-only with react-markdown. Not y-prosemirror/Milkdown. |
     ... Live co-editing, Go, and Proof's *contract* are all kept. | WYSIWYG editing is the cost
```

"WYSIWYG editing is the cost" names a user-visible loss and assigns it without asking. The
correct form was a question in the spec's Decisions table: "D16 amendment: Proof's editor is
ProseMirror-based; using it means Go must translate markdown edits into tree edits (cost: a
Go tree package). Keeping a plain-text editor avoids that but drops Proof's UI and WYSIWYG.
Which?"

## Related

- `docs/solutions/controller/thermonuclear-pair-follows-the-code.md` — the review process that
  ran on this PR and correctly certified the code against the plan; it could not catch a plan
  that had drifted from the user.
- `.superpowers/sdd/2026-09-09-dispatch-native-workspace/rulings.md` — the full ruling list
  (R1–R30) as recorded during planning.
