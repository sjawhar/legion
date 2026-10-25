---
name: dispatch-brainstorming
description: "Use when a session with Dispatch starts a design conversation with a person: brainstorming, /brainstorming or \"let's design\". Also use it there for a spec, an implementation plan, or a change that needs a person's choices. It replaces superpowers' brainstorming and writing-plans. A Legion architect's own spec follows skill://legion-architect instead."
---

# Brainstorming in Dispatch

A Legion architect's own issue spec, a child's too, follows `skill://legion-architect`. A Legion phase worker never writes the spec: it sends the decision to its architect (`skill://legion-worker`).

This skill replaces superpowers' `brainstorming` and `writing-plans`, even when the user names one:

- The spec is the issue's primary document, the plan its `plan.md`. No `docs/superpowers/` files.
- Each design question is a decision block in the spec, never a chat question. A small change gets a short spec.
- Send every ready question at once. Request approval once, at the end.
- Ask in stages: clarifying questions, two or three approaches with a recommendation, the design by section. A question that depends on no open answer goes out at once.
- Once the clarifying questions are answered and the approaches are known, weigh one question yourself before they go into a decision block. Ask whether trying the approaches is quicker than choosing between them: each can be built or prototyped in hours, and what decides between them is how they work, not how they read. If so, have fresh agents build each from the brief alone (ce-bakeoff without its judge; ce-prototype for a page or a flow) and run each through the same check. Show the person what was built and how each did, side by side, in the decision block. Recommend one only where the check settles it; otherwise the person picks. A session without ce-bakeoff or ce-prototype has fresh `task` agents do the same build.
- Copy the spec, plan or spec code into the repository only when the person, the issue, its spec or the repository asks.

Before the first version, read the code and documents the design touches. Before the first version, also read the `skill://dispatch` sections "Writing a spec", "Decision blocks", "Writing for the human" and "Approval of a spec".

## Where the spec lives

1. Search first (`skill://dispatch-first`). If an issue tracks the work, extend its primary document with `dispatch doc-edit`, keeping the human's text.
2. Else run `dispatch issue --project <KEY> --title '<title>' --spec-file -` in the project a search hit names.
3. If no hit names a project, ask the human in chat which one (`dispatch projects` lists them). This is a filing question, not a design question.
4. Use only `dispatch` commands. Never get the project from the environment, a configuration file or the Dispatch HTTP API.

## Each turn

1. **First version: only what the human settled.** Write the problem, its evidence, the human's words and the facts the questions need. Add each ready question as a block at the end of its section. Write no design, no default you chose, and no recommendation outside a block. A point the human did not state goes in a block or stays out, however obvious. A flagged inference waits too.
2. **Reply to each comment in its thread** (`dispatch comment --reply-to <comment id>`), then rewrite what it changes. When the next move on an open ask is yours, such as an approval request to revise, use `--reply-to-ask <ask id> --turn agent`.
3. **Each next version keeps answered blocks in place** and rewrites every passage a decision changes. The spec records the current design and requirements, never how you got there. Then add the next sections and their ready questions.
4. **A comment that answers a question settles it.** Write the answer into the text at once, as "Writing a spec" says. Close the block with `dispatch resolve-ask` if it is still open.
5. **Request approval** when nothing in the spec is new to the human, as "Approval of a spec" says.

## Coming to terms

Explain plainly what the code does now, naming each tool, event and route exactly, and ask. Neither model is true at the start: the human's reactions give their model and its corrections.

## Handing over a spec or plan

1. Start your chat reply with:
   1. Each decision the human must make: options, trade-offs and your recommendation, clear without the document. The human answers in the block.
   2. Each point the human has not yet settled, one line each, with a line reference.
   3. The link.
2. The reply carries only what is new. The document stays the full current design.
3. If no decision is open and nothing is new, do not ask for review in the reply. Say the document records what you agreed. Turn step 5 still applies.
4. When you ask for review, say "ready for review", never "complete". Put the uncertainty first and show your reasoning.

## The plan

- Write it after the spec's approval. With no spec (a bug fix with no design decision), write it when you can plan.
- Upload it with `dispatch artifact --issue <KEY> --name plan.md --content-file -`. Each upload is a new version.
- Follow all `writing-plans` content rules: file structure, task size, small steps, no placeholders, the header, its self-review.
- Make technical choices inside the approved design in the plan, with no question. A change to a human's decision goes back to the spec as a block, with its evidence.
- If the human says "plan only", stop after the plan.
- To execute, read it with `dispatch doc-read --issue <KEY> --artifact plan.md`, then follow `executing-plans` or `subagent-driven-development`.
