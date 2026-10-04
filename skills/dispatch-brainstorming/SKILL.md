---
name: dispatch-brainstorming
description: "Use in a session with Dispatch tools whenever a design conversation starts: brainstorming or /brainstorming, \"let's design\", writing or changing a spec, writing an implementation plan, or a feature request or change that needs the human's choices. In a session with Dispatch it replaces superpowers' brainstorming and writing-plans."
---

# Brainstorming in Dispatch

In a session with Dispatch, a design conversation happens in its issue's spec, and the plan that
follows is a document on the same issue. This skill replaces superpowers' `brainstorming` and
`writing-plans` there, even when the user invokes one of them by name:

- No design question goes to chat. Every question is a decision block in the spec, and your reply
  in chat links the issue and asks nothing.
- There is no limit of one question per message. Every question that is ready goes out at once.
- There is no approval after each section, only the one at the end.
- A small change gets a short spec, never a design in chat: its size shortens the spec and changes
  nothing about where the conversation happens.
- No spec or plan file goes in the repository (`docs/superpowers/specs/`,
  `docs/superpowers/plans/`, `docs/plans/`): the spec is the issue's primary document and the plan
  is its `plan.md` document.

The brainstorming stages still shape what you ask: clarifying questions, then two or three
approaches with a recommendation, then the design section by section. A question that depends on
no open answer goes out at once, whatever its stage.

Read `skill://dispatch` before the first version: its "Writing a spec", "Decision blocks",
"Writing for the human" and "Approval of a spec" sections define the spec's shape, each block's
shape, and when approval is requested. A Legion phase worker never writes the spec; it sends a
decision to its architect (`skill://legion-worker`).

## Where the spec lives

Search Dispatch first, as `skill://dispatch-first` says. When an issue already tracks the work, its
primary document is the spec: extend it in place with `dispatch_doc_edit`, keeping the human's own
text. Otherwise create the issue with its first version as `spec`:

```ts
dispatch_issue({ project, title, spec })
```

Before the first version, read the code and documents the design touches, so the facts you write
are ones you checked.

## Each turn

1. **Start with what is established.** The first version holds only what the conversation has
   established: the problem and its evidence, what the human has said in their own words, the
   facts the next questions need, and each question that is ready, as a decision block at the end
   of the section that sets it up. Write nothing past those questions: no design, no defaults you
   chose, no "what we will build", and no recommendation outside a decision block's own text. A
   point the human has not stated, however obvious it seems (what a word in the request means, how
   an edge case behaves, what a change does to an existing command), goes inside a decision block
   or stays out.
2. **The human answers or comments.** Reply to each human comment in its thread
   (`dispatch_comment` with `reply_to`), then rewrite the passage the answer or the comment changes.
   Under an open ask whose next move is yours, such as the approval request you must revise or
   hand back, reply with `reply_to_ask` and `turn: "agent"` instead: a `reply_to` reply takes the
   default turn, which hands the request back to the human unchanged, and a call with a corrected
   `summary` is refused while it waits on them.
3. **Each next version folds the answers in and adds what they open.** Keep each answered
   decision block where it is, fold its answer into the surrounding text in the human's words with
   the date (an answer that is only a chosen option as
   `Sami chose "Commit author" on the question below (2026-10-02)`), then add the next sections,
   each with its question. Every question that is ready goes out at once, each as a decision block
   at the end of the section that sets it up; a question waits only when it depends on an answer
   still open.
4. **A comment that answers a question settles it** as surely as the block does. Fold it into the
   text at once, and close the block with `dispatch_resolve_ask` if the human has not.
5. **Request approval at the end, not after each section, when nothing in the spec is new to the
   human:** every block settled, every comment answered, and every point they have not agreed to,
   however small, either put to them first as its own decision block or, when it is yours to
   decide, taken out of the spec and made where the work happens. An inference you cannot defend in
   a decision block comes out of the spec. The request carries nothing new ("Approval of a spec" in
   `skill://dispatch`).

## Coming to terms

The conversation comes to terms in both directions. Explain what the code does today, plainly
enough for the human to react to and with each tool, event and route it uses named exactly (as
"Writing for the human" in `skill://dispatch` says), and ask; the human's model comes out of those
reactions, and so do corrections to it. Neither your model nor theirs is the starting truth.

## The plan

Once the spec is approved, the implementation plan is the issue's `plan.md` document, never its
spec and never a repository file:

```ts
dispatch_artifact({ issue, name: "plan.md", content })
```

Uploading `plan.md` again writes its next version. The plan holds what `writing-plans` would put in
its file: the files each task touches, the tests, and the order. Technical choices inside the
approved design are made in the plan and need no question. A plan never changes a decision the
human made in the spec: a change to one goes back to the spec as a decision block, with the evidence
for it.

## A worked example

Take the spec for the secrets broker's identity model. Its first version held the problem, the
human's words, what exists today, and the one question that was ready then; it also called itself
"a conversation", which the human struck as commentary. Each later version folds the answers in
with the human's words and date and adds the questions they open. Its first approval request, on
the spec's version 35, named four inferences the human had never discussed; the human rejected it,
and each of the four was then either put to the human as its own decision block or taken out of
the spec.
